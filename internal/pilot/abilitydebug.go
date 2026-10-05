package pilot

import (
	"context"
	"errors"
	"sync"
	"time"
)

type AbilityDebugExecution struct {
	ID                string            `json:"id"`
	RobotID           string            `json:"robot_id"`
	AbilityInstanceID string            `json:"ability_instance_id"`
	TaskName          string            `json:"task_name"`
	InvocationID      string            `json:"invocation_id"`
	FrameworkTaskID   string            `json:"framework_task_id,omitempty"`
	Status            string            `json:"status"`
	Input             map[string]any    `json:"input,omitempty"`
	Feedback          []AbilityFeedback `json:"feedback,omitempty"`
	Observations      []map[string]any  `json:"observations,omitempty"`
	Result            map[string]any    `json:"result,omitempty"`
	Error             map[string]any    `json:"error,omitempty"`
	Revision          int64             `json:"revision"`
	StartedAt         time.Time         `json:"started_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type AbilityDebugEventSink func(AbilityDebugExecution)

// AbilityDebugService 只在 Pilot 本地调试当前 Robot 的精确 Ability 实例。
// StartTask 连接结果未知时不会重试；恢复只查询既有 invocation。
type AbilityDebugService struct {
	RobotID   string
	Allowed   bool
	Client    AbilityClient
	Store     AbilityDebugStore
	RobotBusy func(string) bool
	Events    AbilityDebugEventSink
	Interval  time.Duration

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func NewAbilityDebugService(robotID string, allowed bool, client AbilityClient, store AbilityDebugStore) *AbilityDebugService {
	return &AbilityDebugService{RobotID: robotID, Allowed: allowed, Client: client, Store: store,
		Interval: 200 * time.Millisecond, active: make(map[string]context.CancelFunc)}
}

func (s *AbilityDebugService) Start(ctx context.Context, request AbilityDebugExecution) (AbilityDebugExecution, error) {
	if !s.Allowed {
		return request, errors.New("RobotDeployment 未允许 Ability 调试")
	}
	if request.ID == "" || request.AbilityInstanceID == "" || request.TaskName == "" {
		return request, errors.New("Ability debug id、instance 和 task 必填")
	}
	if request.RobotID != "" && request.RobotID != s.RobotID {
		return request, ErrRobotNotFound
	}
	request.RobotID = s.RobotID
	if existing, err := s.Store.GetAbilityDebug(request.ID); err == nil {
		return existing, nil
	}

	s.mu.Lock()
	if s.busyLocked() || (s.RobotBusy != nil && s.RobotBusy(s.RobotID)) {
		s.mu.Unlock()
		return request, ErrRobotBusy
	}
	now := time.Now().UTC()
	request.InvocationID = request.ID
	request.Status = "starting"
	request.Revision = 1
	request.StartedAt = now
	request.UpdatedAt = now
	if request.Input == nil {
		request.Input = make(map[string]any)
	}
	request.Input["invocation_id"] = request.InvocationID
	request.Input["robot_id"] = request.RobotID
	if err := s.Store.SaveAbilityDebug(request); err != nil {
		s.mu.Unlock()
		return request, err
	}
	s.mu.Unlock()
	s.report(request)

	task, err := s.Client.StartTask(ctx, request.AbilityInstanceID, request.TaskName, request.Input)
	if err != nil {
		var rejected *AbilityStartRejectedError
		if errors.As(err, &rejected) {
			request.Status = "failed"
			request.Error = map[string]any{"code": "ABILITY_START_REJECTED", "message": err.Error()}
		} else if state, reconcileErr := s.Client.GetExecution(ctx, request.AbilityInstanceID, request.InvocationID, 0); reconcileErr == nil && confirmedAbilityTerminal(state.Status) {
			request = applyDebugState(request, state)
		} else {
			request.Status = "interrupted"
			request.Error = map[string]any{"code": "ABILITY_START_UNCONFIRMED", "message": err.Error()}
		}
		request.Revision++
		request.UpdatedAt = time.Now().UTC()
		_ = s.Store.SaveAbilityDebug(request)
		s.report(request)
		return request, err
	}
	request.FrameworkTaskID = task.TaskID
	request.Status = "running"
	request.Revision++
	request.UpdatedAt = time.Now().UTC()
	if err := s.Store.SaveAbilityDebug(request); err != nil {
		return request, err
	}
	s.report(request)
	s.startMonitor(request.ID)
	return cloneAbilityDebug(request), nil
}

func (s *AbilityDebugService) Get(id string) (AbilityDebugExecution, error) {
	return s.Store.GetAbilityDebug(id)
}

func (s *AbilityDebugService) Stop(ctx context.Context, id, reason string) (AbilityDebugExecution, error) {
	execution, err := s.Store.GetAbilityDebug(id)
	if err != nil {
		return execution, err
	}
	// An interrupted stop still owns the robot. Allow an explicit retry to
	// obtain fresh stop evidence from the original Ability invocation.
	if abilityDebugTerminal(execution.Status) && execution.Status != "interrupted" {
		return execution, nil
	}
	execution.Status = "stopping"
	execution.Revision++
	execution.UpdatedAt = time.Now().UTC()
	_ = s.Store.SaveAbilityDebug(execution)
	s.report(execution)
	state, err := s.Client.StopExecution(ctx, execution.AbilityInstanceID, execution.InvocationID, reason)
	if err != nil {
		execution.Status = "interrupted"
		execution.Error = map[string]any{"code": "STOP_UNCONFIRMED", "message": err.Error()}
	} else {
		execution = applyDebugState(execution, state)
		if execution.Status != "stopped" {
			execution.Status = "interrupted"
			execution.Error = map[string]any{"code": "STOP_UNCONFIRMED", "message": "Ability 未返回 stopped 证据"}
		}
	}
	execution.Revision++
	execution.UpdatedAt = time.Now().UTC()
	_ = s.Store.SaveAbilityDebug(execution)
	s.cancelMonitor(id)
	s.report(execution)
	return cloneAbilityDebug(execution), err
}

// Recover 只恢复 GetExecution 轮询，不调用业务 Task start。
func (s *AbilityDebugService) Recover() error {
	items, err := s.Store.ListActiveAbilityDebug()
	if err != nil {
		return err
	}
	for _, item := range items {
		s.report(item)
		if item.Status != "interrupted" {
			s.startMonitor(item.ID)
		}
	}
	return nil
}

// ReportCurrent 在 Pilot 重新连接 Server 后补发仍占用 Robot 的调试状态。
func (s *AbilityDebugService) ReportCurrent() error {
	items, err := s.Store.ListActiveAbilityDebug()
	if err != nil {
		return err
	}
	for _, item := range items {
		s.report(item)
	}
	return nil
}

func (s *AbilityDebugService) startMonitor(id string) {
	s.mu.Lock()
	if _, exists := s.active[id]; exists {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.active[id] = cancel
	s.mu.Unlock()
	go s.monitor(ctx, id)
}

func (s *AbilityDebugService) monitor(ctx context.Context, id string) {
	interval := s.Interval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	defer s.cancelMonitor(id)
	for {
		execution, err := s.Store.GetAbilityDebug(id)
		if err != nil || abilityDebugTerminal(execution.Status) {
			return
		}
		state, pollErr := s.Client.GetExecution(ctx, execution.AbilityInstanceID, execution.InvocationID, 0)
		if pollErr != nil {
			if ctx.Err() != nil {
				return
			}
			execution.Status = "interrupted"
			execution.Error = map[string]any{"code": "ABILITY_UNAVAILABLE", "message": pollErr.Error()}
		} else {
			// StopExecution 可能与本次查询并发。重新读取状态，避免迟到的
			// running 响应覆盖已经持久化的 stopping/stopped。
			latest, latestErr := s.Store.GetAbilityDebug(id)
			if latestErr != nil || latest.Status == "stopping" || abilityDebugTerminal(latest.Status) {
				return
			}
			execution = latest
			execution = applyDebugState(execution, state)
		}
		execution.Revision++
		execution.UpdatedAt = time.Now().UTC()
		_ = s.Store.SaveAbilityDebug(execution)
		s.report(execution)
		if abilityDebugTerminal(execution.Status) {
			return
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *AbilityDebugService) cancelMonitor(id string) {
	s.mu.Lock()
	cancel := s.active[id]
	delete(s.active, id)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *AbilityDebugService) busyLocked() bool {
	items, err := s.Store.ListActiveAbilityDebug()
	return err != nil || len(items) > 0
}

func (s *AbilityDebugService) report(execution AbilityDebugExecution) {
	if s.Events != nil {
		s.Events(cloneAbilityDebug(execution))
	}
}

func applyDebugState(execution AbilityDebugExecution, state AbilityExecution) AbilityDebugExecution {
	execution.Status = string(normalizeActionStatus(state.Status))
	execution.Feedback = append([]AbilityFeedback(nil), state.Feedback...)
	execution.Observations = cloneMaps(state.Observations)
	execution.Result = cloneMap(state.Result)
	execution.Error = cloneMap(state.Error)
	return execution
}

func abilityDebugTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "stopped", "interrupted":
		return true
	default:
		return false
	}
}

func cloneAbilityDebug(source AbilityDebugExecution) AbilityDebugExecution {
	result := source
	result.Input = cloneMap(source.Input)
	result.Feedback = append([]AbilityFeedback(nil), source.Feedback...)
	result.Observations = cloneMaps(source.Observations)
	result.Result = cloneMap(source.Result)
	result.Error = cloneMap(source.Error)
	return result
}
