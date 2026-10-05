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

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	PlanProposalStatusReady     = "ready"
	PlanProposalStatusApproved  = "approved"
	PlanProposalStatusDiscarded = "discarded"
)

// PlanProposal 是对话中当前可审阅的计划，不是可执行 Workflow。结构化计划
// 决定批准后创建哪些 Task，Markdown 只负责展示，Framework 从不反向解析
// Markdown 来驱动执行。
type PlanProposal struct {
	ID               string          `json:"id"`
	ProjectID        string          `json:"project_id"`
	ConversationID   string          `json:"conversation_id"`
	Revision         int64           `json:"revision"`
	Status           string          `json:"status"`
	Goal             string          `json:"goal"`
	Summary          string          `json:"summary"`
	ApprovedScope    json.RawMessage `json:"approved_scope"`
	StructuredPlan   json.RawMessage `json:"structured_plan"`
	DocumentMarkdown string          `json:"document_markdown"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

const planProposalColumns = `id,project_id,conversation_id,revision,status,goal,summary,
	approved_scope_json,structured_plan_json,document_markdown,created_at,updated_at`

type planProposalScanner interface{ Scan(...any) error }

func scanPlanProposal(row planProposalScanner) (PlanProposal, error) {
	var value PlanProposal
	var scope, plan string
	err := row.Scan(&value.ID, &value.ProjectID, &value.ConversationID,
		&value.Revision, &value.Status, &value.Goal, &value.Summary, &scope, &plan,
		&value.DocumentMarkdown, &value.CreatedAt, &value.UpdatedAt)
	value.ApprovedScope = json.RawMessage(scope)
	value.StructuredPlan = json.RawMessage(plan)
	return value, err
}

func NewPlanProposalID() string { return "plan-" + uuid.NewString() }

// SubmitPlanProposal 原子创建或修订一份完整、可审阅的 Proposal。模型尚未形成
// Task 时不会调用这里；如果本轮工具参数非法或模型流取消，上一 revision 保持
// 原样。用户批准前数据库中也绝不会出现可执行 Workflow。
func (s *Store) SubmitPlanProposal(projectID, conversationID string, requested WorkflowDraft,
	summary string, approvedScope json.RawMessage, document string, now time.Time) (PlanProposal, error) {
	proposal, _, err := s.SubmitPlanProposalForRun("", projectID, conversationID, requested,
		summary, approvedScope, document, now)
	return proposal, err
}

// SubmitPlanProposalForRun 让 Leader 已接受的提交持久且幂等：同一 Run 的第二次
// 调用绝不会产生新 revision，包括其 Workflow 已被批准之后。返回的 Proposal 始终
// 是库中当前那一行，因此 Revision、Status 与文档描述的是同一个版本——即使期间
// 已被其他 Run 修订或已被批准；reused 表示该 Run 之前已经提交过。
// 不同 Run 仍可有意修订一份 ready 的 Proposal。
func (s *Store) SubmitPlanProposalForRun(runID, projectID, conversationID string, requested WorkflowDraft,
	summary string, approvedScope json.RawMessage, document string, now time.Time) (PlanProposal, bool, error) {
	if strings.TrimSpace(requested.Goal) == "" {
		return PlanProposal{}, false, fmt.Errorf("goal 不能为空: %w", ErrInvalidState)
	}
	if err := validateDraft(requested); err != nil {
		return PlanProposal{}, false, err
	}
	if len(requested.Tasks) == 0 {
		return PlanProposal{}, false, ErrInvalidState
	}
	session, err := s.GetChatSession(conversationID)
	if err != nil || session.ProjectID != projectID || session.ArchivedAt != nil {
		return PlanProposal{}, false, ErrNotFound
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return PlanProposal{}, false, err
	}
	if project.ArchivedAt != nil {
		return PlanProposal{}, false, ErrProjectArchived
	}
	if !project.IsActive {
		return PlanProposal{}, false, ErrProjectInactive
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PlanProposal{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if runID != "" {
		var existingID string
		err := tx.QueryRow(`SELECT proposal_id FROM plan_proposal_submissions WHERE run_id=?`,
			runID).Scan(&existingID)
		if err == nil {
			// 刻意不再把 Revision 改写回该 Run 当初提交的值：库中这一行才是权威
			// 版本，期间可能已被其他 Run 修订或已批准。混用“旧 revision + 新内容/
			// 状态”会返回自相矛盾的对象（如 revision=1 却 status=approved），调用方
			// 拿它去批准还会直接 revision 冲突。绑定表里的 revision 只作审计。
			proposal, getErr := scanPlanProposal(tx.QueryRow(`SELECT `+planProposalColumns+
				` FROM plan_proposals WHERE id=?`, existingID))
			if getErr != nil {
				return PlanProposal{}, false, getErr
			}
			if proposal.ProjectID != projectID || proposal.ConversationID != conversationID {
				return PlanProposal{}, false, ErrInvalidState
			}
			return proposal, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return PlanProposal{}, false, err
		}
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM workflows WHERE project_id=?
		AND status IN ('pending','running','paused','stopping')`, projectID).Scan(&count); err != nil {
		return PlanProposal{}, false, err
	}
	if count != 0 {
		return PlanProposal{}, false, ErrWorkflowExists
	}
	encoded, err := json.Marshal(requested)
	if err != nil {
		return PlanProposal{}, false, err
	}
	active, activeErr := scanPlanProposal(tx.QueryRow(`SELECT `+planProposalColumns+
		` FROM plan_proposals WHERE project_id=? AND status='ready'`, projectID))
	var proposalID string
	var submittedRevision int64
	switch {
	case activeErr == nil:
		if active.ConversationID != conversationID {
			return PlanProposal{}, false, ErrWorkflowExists
		}
		result, updateErr := tx.Exec(`UPDATE plan_proposals SET revision=revision+1,
			goal=?,summary=?,approved_scope_json=?,structured_plan_json=?,document_markdown=?,updated_at=?
			WHERE id=? AND revision=? AND status='ready'`, strings.TrimSpace(requested.Goal), strings.TrimSpace(summary),
			jsonOr(approvedScope, "{}"), string(encoded), document, now, active.ID, active.Revision)
		if updateErr != nil {
			return PlanProposal{}, false, updateErr
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return PlanProposal{}, false, ErrRevisionConflict
		}
		proposalID = active.ID
		submittedRevision = active.Revision + 1
	case errors.Is(activeErr, sql.ErrNoRows):
		proposalID = NewPlanProposalID()
		submittedRevision = 1
		_, err = tx.Exec(`INSERT INTO plan_proposals (`+planProposalColumns+`)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`, proposalID, projectID,
			conversationID, PlanProposalStatusReady, strings.TrimSpace(requested.Goal), strings.TrimSpace(summary),
			jsonOr(approvedScope, "{}"), string(encoded), document, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return PlanProposal{}, false, ErrWorkflowExists
			}
			return PlanProposal{}, false, err
		}
	default:
		return PlanProposal{}, false, activeErr
	}
	if runID != "" {
		if _, err := tx.Exec(`INSERT INTO plan_proposal_submissions
			(run_id,proposal_id,proposal_revision,created_at) VALUES (?,?,?,?)`,
			runID, proposalID, submittedRevision, now); err != nil {
			return PlanProposal{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PlanProposal{}, false, err
	}
	proposal, err := s.GetPlanProposal(proposalID)
	return proposal, false, err
}

func (s *Store) GetPlanProposal(id string) (PlanProposal, error) {
	value, err := scanPlanProposal(s.db.QueryRow(`SELECT `+planProposalColumns+
		` FROM plan_proposals WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PlanProposal{}, ErrNotFound
	}
	return value, err
}

func (s *Store) GetActivePlanProposal(projectID string) (PlanProposal, error) {
	value, err := scanPlanProposal(s.db.QueryRow(`SELECT `+planProposalColumns+
		` FROM plan_proposals WHERE project_id=? AND status='ready'
		ORDER BY updated_at DESC LIMIT 1`, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return PlanProposal{}, ErrNotFound
	}
	return value, err
}

func (s *Store) DiscardPlanProposal(id string, expectedRevision int64,
	now time.Time) (PlanProposal, error) {
	result, err := s.db.Exec(`UPDATE plan_proposals SET status=?,revision=revision+1,
		updated_at=? WHERE id=? AND revision=? AND status='ready'`,
		PlanProposalStatusDiscarded, now, id, expectedRevision)
	if err != nil {
		return PlanProposal{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return PlanProposal{}, ErrRevisionConflict
	}
	return s.GetPlanProposal(id)
}

// ApprovePlanProposal 是 Proposal → Workflow 的唯一事务边界。精确 revision、
// Project 活动状态、Task DAG 与结构化计划在同一个事务中确认；成功前数据库
// 不会出现半个 Workflow，也不会要求提前生成全部 SubTask。
func (s *Store) ApprovePlanProposal(id string, expectedRevision int64,
	now time.Time) (WorkflowView, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return WorkflowView{}, err
	}
	defer func() { _ = tx.Rollback() }()
	proposal, err := scanPlanProposal(tx.QueryRow(`SELECT `+planProposalColumns+
		` FROM plan_proposals WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowView{}, ErrNotFound
	}
	if err != nil {
		return WorkflowView{}, err
	}
	if proposal.Revision != expectedRevision || proposal.Status != PlanProposalStatusReady {
		return WorkflowView{}, ErrRevisionConflict
	}
	var draft WorkflowDraft
	if err := json.Unmarshal(proposal.StructuredPlan, &draft); err != nil {
		return WorkflowView{}, fmt.Errorf("Plan Proposal 结构损坏: %w", err)
	}
	if err := validateDraft(draft); err != nil {
		return WorkflowView{}, err
	}
	if len(draft.Tasks) == 0 {
		return WorkflowView{}, ErrInvalidState
	}
	project, err := scanProject(tx.QueryRow(`SELECT `+projectSelectColumns+` FROM projects WHERE id=?`,
		proposal.ProjectID))
	if err != nil {
		return WorkflowView{}, err
	}
	if project.ArchivedAt != nil {
		return WorkflowView{}, ErrProjectArchived
	}
	if !project.IsActive {
		return WorkflowView{}, ErrProjectInactive
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM workflows WHERE project_id=?
		AND status IN ('pending','running','paused','stopping')`, proposal.ProjectID).Scan(&count); err != nil {
		return WorkflowView{}, err
	}
	if count != 0 {
		return WorkflowView{}, ErrWorkflowExists
	}
	workflow := Workflow{
		ID: NewWorkflowID(), ProjectID: proposal.ProjectID,
		ConversationID: proposal.ConversationID, Goal: strings.TrimSpace(draft.Goal),
		ApprovedScope:      json.RawMessage(jsonOr(proposal.ApprovedScope, "{}")),
		Constraints:        json.RawMessage(jsonOr(draft.Constraints, "[]")),
		CompletionCriteria: json.RawMessage(jsonOr(draft.CompletionCriteria, "[]")),
		MapScope:           json.RawMessage(jsonOr(draft.MapScope, "{}")),
		Status:             WorkflowStatusRunning, Revision: 1, ConfirmedRevision: 1,
		CreatedAt: now, StartedAt: &now, UpdatedAt: now,
	}
	_, err = tx.Exec(`INSERT INTO workflows (`+workflowColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', 1, 1, '', ?, ?, ?, NULL)`,
		workflow.ID, workflow.ProjectID, workflow.ConversationID, workflow.Goal,
		string(workflow.ApprovedScope), string(workflow.Constraints),
		string(workflow.CompletionCriteria), string(workflow.MapScope), workflow.Status, now, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return WorkflowView{}, ErrWorkflowExists
		}
		return WorkflowView{}, err
	}
	if err := replacePlanTx(tx, workflow, draft, now); err != nil {
		return WorkflowView{}, err
	}
	if _, err := tx.Exec(`UPDATE projects SET mode=?,revision=revision+1,updated_at=?
		WHERE id=? AND is_active=1 AND archived_at IS NULL`, ProjectModeRunning, now,
		proposal.ProjectID); err != nil {
		return WorkflowView{}, err
	}
	result, err := tx.Exec(`UPDATE plan_proposals SET status=?,revision=revision+1,
		updated_at=? WHERE id=? AND revision=? AND status=?`,
		PlanProposalStatusApproved, now, id, expectedRevision, PlanProposalStatusReady)
	if err != nil {
		return WorkflowView{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return WorkflowView{}, ErrRevisionConflict
	}
	if err := saveWorkflowRevisionTx(tx, workflow.ID, workflow.Revision, now); err != nil {
		return WorkflowView{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowView{}, err
	}
	return s.GetWorkflowView(workflow.ID)
}
