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
	"errors"
	"sync"
)

// ActionJournal 先保存 Action 与 invocation 的对应关系，再触发 Ability。
// 进程恢复只能查询已有 invocation，不能重新发送物理动作。
type ActionJournal interface {
	GetAction(skillExecutionID, key string) (ActionExecution, error)
	GetActionByID(id string) (ActionExecution, error)
	SaveAction(execution ActionExecution) error
	ListActiveActions() ([]ActionExecution, error)
}

var errJournalRecordNotFound = errors.New("pilot journal record not found")

type MemoryActionJournal struct {
	mu      sync.RWMutex
	byID    map[string]ActionExecution
	bySkill map[string]string
}

func NewMemoryActionJournal() *MemoryActionJournal {
	return &MemoryActionJournal{byID: make(map[string]ActionExecution), bySkill: make(map[string]string)}
}

func (j *MemoryActionJournal) GetAction(skillExecutionID, key string) (ActionExecution, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	id := j.bySkill[skillExecutionID+"\x00"+key]
	value, ok := j.byID[id]
	if !ok {
		return ActionExecution{}, errJournalRecordNotFound
	}
	return cloneActionExecution(value), nil
}

func (j *MemoryActionJournal) GetActionByID(id string) (ActionExecution, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	value, ok := j.byID[id]
	if !ok {
		return ActionExecution{}, errJournalRecordNotFound
	}
	return cloneActionExecution(value), nil
}

func (j *MemoryActionJournal) SaveAction(execution ActionExecution) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	key := execution.SkillExecutionID + "\x00" + execution.Key
	if existingID := j.bySkill[key]; existingID != "" && existingID != execution.ID {
		return ErrActionKeyConflict
	}
	j.byID[execution.ID] = cloneActionExecution(execution)
	j.bySkill[key] = execution.ID
	return nil
}

func (j *MemoryActionJournal) ListActiveActions() ([]ActionExecution, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	result := make([]ActionExecution, 0)
	for _, item := range j.byID {
		if !actionTerminal(item.Status) {
			result = append(result, cloneActionExecution(item))
		}
	}
	return result, nil
}
