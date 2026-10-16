package workflow

import (
	"context"
	"encoding/json"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

type robotPreparationWait struct {
	Category            string `json:"category"`
	Message             string `json:"message"`
	SourceInteractionID string `json:"source_interaction_id,omitempty"`
}

func (s *Service) pauseRobotPreparation(task store.Task, cause error, answer *store.Interaction) error {
	wait := robotPreparationWait{Category: "resource", Message: cause.Error()}
	if answer != nil {
		wait.SourceInteractionID = answer.ID
	}
	result, _ := json.Marshal(wait)
	for _, step := range mustSubTasks(s.st, task.ID) {
		if step.Status != store.TaskStatusRunning || step.Kind != "robot_skill" {
			continue
		}
		if _, err := s.st.TransitionRobotPreparation(task, step, false, result, time.Now().UTC()); err != nil {
			return err
		}
		s.publishCurrent(task.WorkflowID, "task.waiting_resource")
		return nil
	}
	return store.ErrInvalidState
}

func (s *Service) resumeRobotPreparation(ctx context.Context, taskID string) error {
	s.mu.Lock()
	stopping := s.shuttingDown
	s.mu.Unlock()
	if stopping {
		return nil
	}
	task, err := s.st.GetTask(taskID)
	if err != nil || task.Status != store.TaskStatusPaused || task.WaitingReason != "waiting_resource" ||
		task.RequiredRole != "robot" || s.robotAvailability == nil ||
		!s.robotAvailability.IsRobotAvailable(task.AssignedRobotID) {
		return err
	}
	view, err := s.st.GetWorkflowView(task.WorkflowID)
	if err != nil || view.Workflow.Status != store.WorkflowStatusRunning ||
		view.Workflow.ConfirmedRevision != view.Workflow.Revision {
		return err
	}
	project, err := s.st.GetProject(view.Workflow.ProjectID)
	if err != nil || !project.IsActive || project.ArchivedAt != nil || project.Mode != store.ProjectModeRunning {
		return err
	}
	if !s.reserveTask(task) {
		return nil
	}
	launched := false
	defer func() {
		if !launched {
			s.releaseTask(task)
		}
	}()
	for _, step := range view.SubTasks {
		if step.TaskID != task.ID || step.Status != store.TaskStatusPaused || step.WaitingReason != "waiting_resource" {
			continue
		}
		var wait robotPreparationWait
		if err := json.Unmarshal(step.Result, &wait); err != nil {
			return err
		}
		var answer *store.Interaction
		if wait.SourceInteractionID != "" {
			saved, err := s.st.GetInteraction(wait.SourceInteractionID)
			if err != nil {
				return err
			}
			if saved.TaskID != task.ID || saved.WorkflowID != task.WorkflowID ||
				(saved.Status != store.InteractionStatusAnswered && saved.Status != store.InteractionStatusCancelled) {
				return store.ErrInvalidState
			}
			answer = &saved
		}
		resumed, err := s.st.TransitionRobotPreparation(task, step, true, step.Result, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := s.launchRunningTask(ctx, project.OwnerID, view.Workflow, resumed, answer); err != nil {
			return err
		}
		launched = true
		s.publishCurrent(task.WorkflowID, "task.resumed")
		return nil
	}
	return nil
}
