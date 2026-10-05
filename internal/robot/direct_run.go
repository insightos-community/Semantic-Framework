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

package robot

import (
	"context"
	"errors"
	"strings"
	"sync"

	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
)

func (s *Service) robotAdmission(robotID string) *sync.Mutex {
	value, _ := s.admissions.LoadOrStore(robotID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (s *Service) ValidateDirectRunScope(projectID, runID, agentID, robotID string) error {
	if projectID == "" || runID == "" || robotID == "" || agentID != "robot:"+robotID {
		return ErrDirectRunScope
	}
	run, err := s.st.GetRunSession(runID)
	if err != nil || run.ProjectID != projectID || run.AgentID != agentID ||
		run.Kind != store.RunKindConversation || run.TaskID != "" || run.WorkflowID != "" ||
		run.Status != store.RunStatusRunning || (run.RobotID != "" && run.RobotID != robotID) {
		return ErrDirectRunScope
	}
	return s.ValidateRobotProject(projectID, robotID)
}

func (s *Service) ValidateRobotProject(projectID, robotID string) error {
	instance, err := s.st.GetLatestRuntimeByRobot(context.Background(), robotID)
	if errors.Is(err, robotruntime.ErrInstanceNotFound) {
		return nil // Manually registered Robots have no managed Project binding.
	}
	if err != nil {
		return err
	}
	if instance.ProjectID != "" && instance.ProjectID != projectID {
		return ErrDirectRunScope
	}
	return nil
}

func (s *Service) cancelDirectRun(runID string) error {
	run, err := s.st.GetRunSession(runID)
	if err != nil {
		return err
	}
	if run.Kind != store.RunKindConversation || run.TaskID != "" || run.WorkflowID != "" || !strings.HasPrefix(run.AgentID, "robot:") {
		return nil
	}
	if run.Status == store.RunStatusRunning || run.Status == store.RunStatusWaitingInput || run.Status == store.RunStatusQueued {
		_, err = s.st.TransitionRunStatus(run.ID, []string{store.RunStatusRunning,
			store.RunStatusWaitingInput, store.RunStatusQueued}, store.RunStatusCancelling, s.now().UTC())
		if errors.Is(err, store.ErrInvalidState) {
			return nil // A concurrent finish/cancel already closed admission.
		}
	}
	return err
}

// StopDirectRun closes admission before stopping existing Skills. It deliberately
// retains the Pilot pointer and the stopping/interrupted execution facts.
func (s *Service) StopDirectRun(ctx context.Context, runID string) error {
	if err := s.cancelDirectRun(runID); err != nil {
		return err
	}
	return s.stopDirectRunExecutions(ctx, runID)
}

// FinishDirectRun is called after the Run's terminal status has been persisted.
// Physical ownership remains with Pilot until its safe-stop evidence arrives.
func (s *Service) FinishDirectRun(ctx context.Context, runID string) error {
	run, err := s.st.GetRunSession(runID)
	if err != nil || run.RobotID == "" {
		return err
	}
	if run.Status != store.RunStatusCompleted && run.Status != store.RunStatusFailed && run.Status != store.RunStatusCancelled {
		return store.ErrInvalidState
	}
	// 正常完成只表示 Agent 已提交 Skill，物理执行继续由 Pilot 持有。
	// 用户取消或 Agent 失败仍走原有安全停止，不能把两种生命周期混为一体。
	if run.Status != store.RunStatusCompleted {
		if err := s.stopDirectRunExecutions(ctx, runID); err != nil {
			return err
		}
	}
	if pilot, err := s.st.GetActiveRobotPilot(run.RobotID); err == nil {
		s.publishPilotView(pilot, "robot.run.finished", "robot")
		s.notifyRobotAvailable(pilot)
	}
	return nil
}

func (s *Service) stopDirectRunExecutions(ctx context.Context, runID string) error {
	executions, err := s.st.ListRobotExecutionsByRun(runID)
	if err != nil {
		return err
	}
	var result error
	for _, execution := range executions {
		if execution.TaskID != "" || (!activeRobotStatus(execution.Status) && execution.Status != "interrupted") {
			continue
		}
		if _, err := s.Stop(ctx, execution.ProjectID, execution.ID, "conversation_run_ended"); err != nil && !errors.Is(err, ErrExecutionNotActive) {
			result = errors.Join(result, err)
		}
	}
	return result
}
