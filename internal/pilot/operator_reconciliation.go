package pilot

import "errors"

// ReconciliationSnapshots includes unknown physical owners, which cannot be
// replayed by Recover but still need an explicit operator stop confirmation.
func (r *SkillRuntime) ReconciliationSnapshots() ([]map[string]any, error) {
	items, err := r.store.ListRecoverableSkillExecutions()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	appendItem := func(item SkillExecution) {
		if !seen[item.ID] {
			items = append(items, item)
			seen[item.ID] = true
		}
	}
	for _, item := range items {
		seen[item.ID] = true
	}
	actions, err := r.runner.journal.ListActiveActions()
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		if !action.Physical || action.Status != ActionInterrupted {
			continue
		}
		item, err := r.store.GetSkillExecution(action.SkillExecutionID)
		if err != nil {
			return nil, err
		}
		appendItem(item)
	}
	r.mu.Lock()
	owners := make([]string, 0, len(r.activeRobots))
	for _, id := range r.activeRobots {
		owners = append(owners, id)
	}
	r.mu.Unlock()
	for _, id := range owners {
		item, err := r.store.GetSkillExecution(id)
		if err != nil {
			return nil, err
		}
		appendItem(item)
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{"execution_id": item.ID, "status": item.Status, "checkpoint": item.Checkpoint, "feedback_cursors": item.FeedbackCursors})
	}
	return result, nil
}

// ConfirmOperatorStop consumes only an audited Server confirmation for this
// exact execution. It never calls an Ability or replays a command.
func (r *SkillRuntime) ConfirmOperatorStop(id, robotID string, confirmation map[string]any) error {
	if confirmation["confirmation_source"] != "operator" || confirmation["safe"] != true ||
		confirmation["hold_confirmed"] != true || stringValue(confirmation["confirmed_by"]) == "" ||
		stringValue(confirmation["confirmation_reason"]) == "" {
		return ErrManualCheckRequired
	}
	execution, err := r.store.GetSkillExecution(id)
	if err != nil {
		return err
	}
	if execution.RobotID != robotID {
		return errors.New("operator confirmation Robot does not match")
	}
	if execution.Status != SkillInterrupted && execution.Status != SkillStopped && execution.Status != SkillFailed {
		return ErrManualCheckRequired
	}
	actions, err := r.runner.journal.ListActiveActions()
	if err != nil {
		return err
	}
	for _, action := range actions {
		if action.SkillExecutionID == id && (action.RobotID != robotID || action.Status != ActionInterrupted) {
			return ErrManualCheckRequired
		}
	}
	// Keep the original interrupted outcome and its diagnostic; attach the
	// independent operator stop evidence without pretending the task succeeded.
	if err := r.store.UpdateSkillStopOutcome(id, confirmation); err != nil {
		return err
	}
	for _, action := range actions {
		if action.SkillExecutionID != id {
			continue
		}
		if _, err := r.runner.ConfirmInterrupted(action.ID, confirmation); err != nil {
			return err
		}
	}
	r.cleanup(id, robotID)
	return nil
}
