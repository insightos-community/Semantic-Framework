// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pilot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type AbilityDebugStore interface {
	GetAbilityDebug(id string) (AbilityDebugExecution, error)
	SaveAbilityDebug(execution AbilityDebugExecution) error
	ListActiveAbilityDebug() ([]AbilityDebugExecution, error)
}

// SkillExecutionStore 保存 Worker 重启所需的输入、checkpoint、终态和待处理 Agent 请求。
type SkillExecutionStore interface {
	GetSkillExecution(id string) (SkillExecution, error)
	SaveSkillExecution(execution SkillExecution) error
	UpdateSkillStopOutcome(id string, outcome map[string]any) error
	ListRecoverableSkillExecutions() ([]SkillExecution, error)
	SaveAgentRequest(request AgentRequest) error
	GetAgentRequest(executionID, decisionKey string) (AgentRequest, error)
}

type MemorySkillExecutionStore struct {
	mu       sync.RWMutex
	items    map[string]SkillExecution
	requests map[string]AgentRequest
	debug    map[string]AbilityDebugExecution
}

func NewMemorySkillExecutionStore() *MemorySkillExecutionStore {
	return &MemorySkillExecutionStore{items: make(map[string]SkillExecution), requests: make(map[string]AgentRequest),
		debug: make(map[string]AbilityDebugExecution)}
}

func (s *MemorySkillExecutionStore) GetSkillExecution(id string) (SkillExecution, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[id]
	if !ok {
		return SkillExecution{}, ErrExecutionNotFound
	}
	return cloneSkillExecution(item), nil
}

func (s *MemorySkillExecutionStore) SaveSkillExecution(execution SkillExecution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.items[execution.ID]; exists && skillTerminal(current.Status) && current.Status != execution.Status {
		// Robot Skill 的终态必须单调。Worker 的反馈、checkpoint 或重连消息可能
		// 比终态晚到，不能让这些较旧快照把 completed/failed/stopped/interrupted
		// 回退成 running，否则心跳会重新把 Robot 标成 busy，甚至重放物理动作。
		return nil
	}
	s.items[execution.ID] = cloneSkillExecution(execution)
	return nil
}

func (s *MemorySkillExecutionStore) UpdateSkillStopOutcome(id string, outcome map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	execution, ok := s.items[id]
	if !ok {
		return ErrExecutionNotFound
	}
	// 只合并停止证据，不用 Worker 通知携带的旧状态覆盖当前终态。
	execution.StopOutcome = cloneMap(outcome)
	s.items[id] = cloneSkillExecution(execution)
	return nil
}

func (s *MemorySkillExecutionStore) ListRecoverableSkillExecutions() ([]SkillExecution, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]SkillExecution, 0)
	for _, item := range s.items {
		if !skillTerminal(item.Status) {
			result = append(result, cloneSkillExecution(item))
		}
	}
	return result, nil
}

func (s *MemorySkillExecutionStore) SaveAgentRequest(request AgentRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[request.ExecutionID+"\x00"+request.DecisionKey] = request
	return nil
}

func (s *MemorySkillExecutionStore) GetAgentRequest(executionID, decisionKey string) (AgentRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	request, ok := s.requests[executionID+"\x00"+decisionKey]
	if !ok {
		return AgentRequest{}, ErrExecutionNotFound
	}
	return request, nil
}

func (s *MemorySkillExecutionStore) GetAbilityDebug(id string) (AbilityDebugExecution, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.debug[id]
	if !ok {
		return AbilityDebugExecution{}, ErrExecutionNotFound
	}
	return cloneAbilityDebug(item), nil
}

func (s *MemorySkillExecutionStore) SaveAbilityDebug(execution AbilityDebugExecution) error {
	s.mu.Lock()
	s.debug[execution.ID] = cloneAbilityDebug(execution)
	s.mu.Unlock()
	return nil
}

func (s *MemorySkillExecutionStore) ListActiveAbilityDebug() ([]AbilityDebugExecution, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]AbilityDebugExecution, 0)
	for _, item := range s.debug {
		if item.Status != "succeeded" && item.Status != "failed" && item.Status != "stopped" {
			result = append(result, cloneAbilityDebug(item))
		}
	}
	return result, nil
}

// SQLiteStore 把 Pilot 状态放进同一个 SQLite 数据库；业务内容以 JSON 保存，表只负责索引与事务。
type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	store := &SQLiteStore{db: db}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS pilot_action_journal (
			id TEXT PRIMARY KEY,
			skill_execution_id TEXT NOT NULL,
			action_key TEXT NOT NULL,
			body BLOB NOT NULL,
			UNIQUE(skill_execution_id, action_key)
		)`,
		`CREATE TABLE IF NOT EXISTS pilot_skill_executions (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			body BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS pilot_agent_requests (
			execution_id TEXT NOT NULL,
			decision_key TEXT NOT NULL,
			body BLOB NOT NULL,
			PRIMARY KEY(execution_id, decision_key)
		)`,
	}
	statements = append(statements, "CREATE TABLE IF NOT EXISTS pilot_ability_debug ("+
		"id TEXT PRIMARY KEY, status TEXT NOT NULL, body BLOB NOT NULL)")
	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			return nil, fmt.Errorf("initialize pilot store: %w", err)
		}
	}
	return store, nil
}

func (s *SQLiteStore) GetAction(skillExecutionID, key string) (ActionExecution, error) {
	return scanAction(s.db.QueryRow(`SELECT body FROM pilot_action_journal WHERE skill_execution_id=? AND action_key=?`, skillExecutionID, key))
}

func (s *SQLiteStore) GetActionByID(id string) (ActionExecution, error) {
	return scanAction(s.db.QueryRow(`SELECT body FROM pilot_action_journal WHERE id=?`, id))
}

func scanAction(row *sql.Row) (ActionExecution, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ActionExecution{}, errJournalRecordNotFound
		}
		return ActionExecution{}, err
	}
	var execution ActionExecution
	if err := json.Unmarshal(body, &execution); err != nil {
		return ActionExecution{}, err
	}
	return execution, nil
}

func (s *SQLiteStore) SaveAction(execution ActionExecution) error {
	body, err := json.Marshal(execution)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO pilot_action_journal(id,skill_execution_id,action_key,body)
		VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`,
		execution.ID, execution.SkillExecutionID, execution.Key, body)
	return err
}

func (s *SQLiteStore) ListActiveActions() ([]ActionExecution, error) {
	rows, err := s.db.Query(`SELECT body FROM pilot_action_journal`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ActionExecution
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item ActionExecution
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		if !actionTerminal(item.Status) {
			result = append(result, item)
		}
	}
	return result, rows.Err()
}

func (s *SQLiteStore) GetSkillExecution(id string) (SkillExecution, error) {
	var body []byte
	if err := s.db.QueryRow(`SELECT body FROM pilot_skill_executions WHERE id=?`, id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillExecution{}, ErrExecutionNotFound
		}
		return SkillExecution{}, err
	}
	var execution SkillExecution
	if err := json.Unmarshal(body, &execution); err != nil {
		return SkillExecution{}, err
	}
	return execution, nil
}

func (s *SQLiteStore) SaveSkillExecution(execution SkillExecution) error {
	body, err := json.Marshal(execution)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO pilot_skill_executions(id,status,body) VALUES(?,?,?)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, body=excluded.body
		WHERE pilot_skill_executions.status NOT IN ('completed','failed','stopped','interrupted')
		   OR pilot_skill_executions.status=excluded.status`, execution.ID, execution.Status, body)
	return err
}

func (s *SQLiteStore) UpdateSkillStopOutcome(id string, outcome map[string]any) error {
	body, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	// json_set 由 SQLite 在一条语句内只修改 StopOutcome。并发 Stop 即使已经
	// 把 status/body 写成 stopped，本通知也不会把旧的 stopping 快照写回。
	result, err := s.db.Exec(
		`UPDATE pilot_skill_executions SET body=json_set(body, ?, json(?)) WHERE id=?`,
		"$.StopOutcome", string(body), id,
	)
	if err != nil {
		return err
	}
	affected, affectedErr := result.RowsAffected()
	if affectedErr != nil || affected == 0 {
		return ErrExecutionNotFound
	}
	return nil
}

func (s *SQLiteStore) ListRecoverableSkillExecutions() ([]SkillExecution, error) {
	rows, err := s.db.Query(`SELECT body FROM pilot_skill_executions WHERE status NOT IN ('completed','failed','stopped','interrupted')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SkillExecution
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item SkillExecution
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) SaveAgentRequest(request AgentRequest) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO pilot_agent_requests(execution_id,decision_key,body) VALUES(?,?,?)
		ON CONFLICT(execution_id,decision_key) DO UPDATE SET body=excluded.body`, request.ExecutionID, request.DecisionKey, body)
	return err
}

func (s *SQLiteStore) GetAgentRequest(executionID, decisionKey string) (AgentRequest, error) {
	var body []byte
	if err := s.db.QueryRow(`SELECT body FROM pilot_agent_requests WHERE execution_id=? AND decision_key=?`, executionID, decisionKey).Scan(&body); err != nil {
		return AgentRequest{}, err
	}
	var request AgentRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return AgentRequest{}, err
	}
	return request, nil
}

func (s *SQLiteStore) GetAbilityDebug(id string) (AbilityDebugExecution, error) {
	var body []byte
	if err := s.db.QueryRow("SELECT body FROM pilot_ability_debug WHERE id=?", id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AbilityDebugExecution{}, ErrExecutionNotFound
		}
		return AbilityDebugExecution{}, err
	}
	var execution AbilityDebugExecution
	if err := json.Unmarshal(body, &execution); err != nil {
		return AbilityDebugExecution{}, err
	}
	return execution, nil
}

func (s *SQLiteStore) SaveAbilityDebug(execution AbilityDebugExecution) error {
	body, err := json.Marshal(execution)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO pilot_ability_debug(id,status,body) VALUES(?,?,?) "+
		"ON CONFLICT(id) DO UPDATE SET status=excluded.status, body=excluded.body", execution.ID, execution.Status, body)
	return err
}

func (s *SQLiteStore) ListActiveAbilityDebug() ([]AbilityDebugExecution, error) {
	rows, err := s.db.Query("SELECT body FROM pilot_ability_debug WHERE status NOT IN ('succeeded','failed','stopped')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AbilityDebugExecution
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item AbilityDebugExecution
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func cloneSkillExecution(source SkillExecution) SkillExecution {
	result := source
	result.Input = cloneMap(source.Input)
	result.Checkpoint = cloneMap(source.Checkpoint)
	result.FeedbackCursors = make(map[string]int64, len(source.FeedbackCursors))
	for key, value := range source.FeedbackCursors {
		result.FeedbackCursors[key] = value
	}
	result.Result = cloneMap(source.Result)
	result.Error = cloneMap(source.Error)
	result.StopOutcome = cloneMap(source.StopOutcome)
	return result
}

func skillTerminal(status SkillExecutionStatus) bool {
	return status == SkillCompleted || status == SkillFailed || status == SkillStopped || status == SkillInterrupted
}
