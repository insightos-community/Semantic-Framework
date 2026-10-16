package pilot

import (
	"context"
	"errors"
	"fmt"
)

// SDKTaskClient adapts the existing start/status/cancel API. Business tasks keep
// their own lifetime; Pilot's persisted Action journal holds the exact task ID.
type SDKTaskClient interface {
	StartSDKTask(context.Context, string, string, map[string]any) (AbilityTask, error)
	QuerySDKTask(context.Context, string, string, bool) (AbilityExecution, error)
	StopSDKTask(context.Context, string, string, bool) (AbilityExecution, error)
}

func (r *Runner) startAbility(ctx context.Context, e ActionExecution, input map[string]any) (AbilityTask, error) {
	if e.ExecutionProtocol != SDKTaskProtocol {
		return r.client.StartTask(ctx, e.InstanceID, e.TaskName, input)
	}
	client, ok := r.client.(SDKTaskClient)
	if !ok {
		return AbilityTask{}, &AbilityStartRejectedError{StatusCode: 400, Message: "SDK task protocol unavailable"}
	}
	return client.StartSDKTask(ctx, e.InstanceID, e.TaskName, input)
}

func (r *Runner) queryAbility(ctx context.Context, e ActionExecution) (AbilityExecution, error) {
	if e.ExecutionProtocol != SDKTaskProtocol {
		return r.client.GetExecution(ctx, e.InstanceID, e.InvocationID, e.FeedbackCursor)
	}
	client, ok := r.client.(SDKTaskClient)
	if !ok || e.FrameworkTaskID == "" {
		return AbilityExecution{}, errors.New("SDK task identity unavailable; execution must not be replayed")
	}
	return client.QuerySDKTask(ctx, e.InstanceID, e.FrameworkTaskID, e.Physical)
}

func (r *Runner) stopAbility(ctx context.Context, e ActionExecution, reason string) (AbilityExecution, error) {
	if e.ExecutionProtocol != SDKTaskProtocol {
		return r.client.StopExecution(ctx, e.InstanceID, e.InvocationID, reason)
	}
	client, ok := r.client.(SDKTaskClient)
	if !ok || e.FrameworkTaskID == "" {
		return AbilityExecution{}, errors.New("SDK task identity unavailable; stop is unconfirmed")
	}
	return client.StopSDKTask(ctx, e.InstanceID, e.FrameworkTaskID, e.Physical)
}

func (c *AbilityFrameworkClient) StartSDKTask(ctx context.Context, instance, task string, input map[string]any) (AbilityTask, error) {
	id, err := c.start(ctx, instance, task, input)
	return AbilityTask{TaskID: id}, err
}

func (c *AbilityFrameworkClient) QuerySDKTask(ctx context.Context, instance, id string, physical bool) (AbilityExecution, error) {
	var raw struct {
		TaskID       string         `json:"task_id"`
		Status       string         `json:"status"`
		Payload      map[string]any `json:"payload"`
		Error        map[string]any `json:"error"`
		StopEvidence map[string]any `json:"stop_evidence"`
	}
	if err := c.post(ctx, instance, "/api/task/status", map[string]any{"task_id": id}, &raw); err != nil {
		return AbilityExecution{}, err
	}
	if raw.TaskID != id {
		return AbilityExecution{}, errors.New("SDK returned another task identity")
	}
	result := AbilityExecution{Result: raw.Payload, Error: raw.Error}
	switch raw.Status {
	case "running":
		result.Status = "running"
	case "cancel_requested":
		result.Status = "stopping"
	case "completed":
		result.Status = "succeeded"
	case "failed":
		result.Status = "failed"
	case "cancelled":
		result.Status = "stopped"
	case "stop_uncertain":
		result.Status = "interrupted"
	default:
		return AbilityExecution{}, fmt.Errorf("unknown SDK task status %q", raw.Status)
	}
	if physical && confirmedAbilityTerminal(result.Status) && raw.StopEvidence["confirmed"] != true {
		result.Status = "interrupted"
		result.Error = map[string]any{"code": "STOP_UNCONFIRMED", "message": "Physical task lacks confirmed stop evidence", "cause": raw.Error}
	}
	return result, nil
}

func (c *AbilityFrameworkClient) StopSDKTask(ctx context.Context, instance, id string, physical bool) (AbilityExecution, error) {
	state, err := c.QuerySDKTask(ctx, instance, id, physical)
	if err != nil || confirmedAbilityTerminal(state.Status) {
		return state, err
	}
	var response map[string]any
	if err := c.post(ctx, instance, "/api/task/cancel", map[string]any{"task_id": id}, &response); err != nil {
		return AbilityExecution{}, err
	}
	return c.QuerySDKTask(ctx, instance, id, physical)
}
