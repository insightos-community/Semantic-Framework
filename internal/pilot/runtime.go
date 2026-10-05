package pilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type SkillStartRequest struct {
	ExecutionID string
	ProjectID   string
	TaskID      string
	SubtaskID   string
	RobotID     string
	SkillName   string
	Version     string
	Input       map[string]any
}

type RuntimeEventSink interface {
	Report(execution SkillExecution, event string, fields map[string]any)
	Log(execution SkillExecution, level, message string, fields map[string]any)
}

type nopEventSink struct{}

func (nopEventSink) Report(SkillExecution, string, map[string]any)      {}
func (nopEventSink) Log(SkillExecution, string, string, map[string]any) {}

type activeSkill struct {
	definition     SkillDefinition
	worker         *WorkerProcess
	cancel         context.CancelFunc
	mu             sync.Mutex
	stopMu         sync.Mutex
	actions        map[string]string
	restarts       int
	stopping       bool
	decisionCancel context.CancelFunc
	// 只记录这个 Worker 是否已经把物理 Action 交给 Ability。输入反序列化、
	// 参数校验等阶段失败时现场没有被改变，不能误报 interrupted 并永久占用 Robot。
	physicalActionStarted bool
}

func (a *activeSkill) stopDecisions() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopping = true
	if a.decisionCancel != nil {
		a.decisionCancel()
	}
}

func (a *activeSkill) currentWorker() *WorkerProcess {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.worker
}

func (a *activeSkill) replaceWorker(worker *WorkerProcess) {
	a.mu.Lock()
	a.worker = worker
	a.mu.Unlock()
}

// SkillRuntime 管理一个 Robot Skill 的完整生命周期；Python 只推进 Stage，物理调用始终留在 Pilot。
type SkillRuntime struct {
	skills       *SkillCatalog
	abilities    *Catalog
	runner       *Runner
	store        SkillExecutionStore
	supervisor   WorkerSupervisor
	agent        AgentGateway
	observations ObservationSource
	artifacts    ArtifactService
	events       RuntimeEventSink
	now          func() time.Time

	mu           sync.Mutex
	active       map[string]*activeSkill
	activeRobots map[string]string
}

// ValidateInput 使用Skill声明的Pydantic input_model做只读预检。Worker在该
// 调用中不会创建Execution、Stage或Action；Skill开发者也无需实现额外校验器。
func (r *SkillRuntime) ValidateInput(ctx context.Context, name, version string,
	input map[string]any) (map[string]any, error) {
	definition, err := r.skills.Resolve(name, version)
	if err != nil {
		return nil, err
	}
	worker, err := r.supervisor.Start(ctx, definition)
	if err != nil {
		return nil, err
	}
	defer func() { _ = worker.Kill() }()
	value, err := worker.Call(ctx, "skill.validate_input", map[string]any{"input": input})
	if err != nil {
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("Worker 输入预检返回非法结果")
	}
	return result, nil
}

// DescribeInput exports the exact installed model without starting a Skill or
// any Action. Do not cache by name alone: robots may install different versions.
func (r *SkillRuntime) DescribeInput(ctx context.Context, name, version string) (map[string]any, error) {
	definition, err := r.skills.Resolve(name, version)
	if err != nil {
		return nil, err
	}
	worker, err := r.supervisor.Start(ctx, definition)
	if err != nil {
		return nil, err
	}
	defer func() { _ = worker.Kill() }()
	if len(worker.inputSchema) == 0 {
		return nil, errors.New("installed Worker did not export input_schema")
	}
	return map[string]any{"name": definition.Name, "version": definition.Version,
		"input_schema": worker.inputSchema}, nil
}

func NewSkillRuntime(skills *SkillCatalog, abilities *Catalog, runner *Runner, store SkillExecutionStore, supervisor WorkerSupervisor, agent AgentGateway, observations ObservationSource, events RuntimeEventSink) *SkillRuntime {
	if store == nil {
		store = NewMemorySkillExecutionStore()
	}
	if events == nil {
		events = nopEventSink{}
	}
	return &SkillRuntime{
		skills: skills, abilities: abilities, runner: runner, store: store, supervisor: supervisor,
		agent: agent, observations: observations, events: events, now: time.Now,
		active: make(map[string]*activeSkill), activeRobots: make(map[string]string),
	}
}

// SetArtifactService 装配 Execution 隔离的 Artifact 解析与发布服务。
func (r *SkillRuntime) SetArtifactService(service ArtifactService) {
	r.artifacts = service
}

// SetEventSink 在常驻进程启动任何 Worker 前装配远程事件出口，用于解决
// RemoteClient 与 Runtime 的构造依赖。运行中不得替换该对象。
func (r *SkillRuntime) SetEventSink(events RuntimeEventSink) {
	if events == nil {
		events = nopEventSink{}
	}
	r.events = events
}

// SkillInUse 供安装管理器拒绝停用或卸载正在运行的固定版本。
func (r *SkillRuntime) SkillInUse(name, version string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, active := range r.active {
		if active.definition.Name == name && active.definition.Version == version {
			return true
		}
	}
	return false
}

// RobotInUse 让本地 Ability 调试在 Server 状态过期时仍受 Robot 互斥保护。
func (r *SkillRuntime) RobotInUse(robotID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeRobots[robotID] != ""
}

func (r *SkillRuntime) Start(ctx context.Context, request SkillStartRequest) (SkillExecution, error) {
	if request.ExecutionID != "" {
		existing, loadErr := r.store.GetSkillExecution(request.ExecutionID)
		if loadErr == nil {
			// Server 在连接恢复或 command.ack 丢失后会重发 execution.start。
			// execution_id 是这次物理执行的幂等键；已经见过的 ID 只能返回原记录，
			// 绝不能再次启动 Worker。未结束的执行由 Recover 负责恢复。
			if existing.RobotID != request.RobotID || existing.SkillName != request.SkillName || existing.SkillVersion != request.Version {
				return SkillExecution{}, fmt.Errorf("execution %s 已绑定到 %s/%s@%s", request.ExecutionID,
					existing.RobotID, existing.SkillName, existing.SkillVersion)
			}
			return cloneSkillExecution(existing), nil
		}
		if !errors.Is(loadErr, ErrExecutionNotFound) {
			return SkillExecution{}, loadErr
		}
	}
	definition, err := r.skills.Resolve(request.SkillName, request.Version)
	if err != nil {
		return SkillExecution{}, err
	}
	actions := append(append([]ActionRef{}, definition.RequiredActions...), definition.StopActions...)
	if err := r.abilities.ValidateRequired(request.RobotID, actions); err != nil {
		return SkillExecution{}, err
	}

	r.mu.Lock()
	if current := r.activeRobots[request.RobotID]; current != "" || r.runner.physicalOwner(request.RobotID) != "" {
		r.mu.Unlock()
		return SkillExecution{}, ErrRobotBusy
	}
	executionID := request.ExecutionID
	if executionID == "" {
		executionID = "skill-" + uuid.NewString()
	}
	r.activeRobots[request.RobotID] = executionID
	r.mu.Unlock()

	now := r.now()
	execution := SkillExecution{
		ID: executionID, ProjectID: request.ProjectID, TaskID: request.TaskID, SubtaskID: request.SubtaskID,
		RobotID: request.RobotID, SkillName: definition.Name, SkillVersion: definition.Version,
		Status: SkillStarting, Input: cloneMap(request.Input), FeedbackCursors: make(map[string]int64), CreatedAt: now, UpdatedAt: now,
	}
	if err := r.store.SaveSkillExecution(execution); err != nil {
		r.releaseRobot(request.RobotID, executionID)
		return SkillExecution{}, err
	}
	workerContext, cancel := context.WithCancel(context.Background())
	worker, err := r.supervisor.Start(workerContext, definition)
	if err != nil {
		cancel()
		execution.Status = SkillFailed
		execution.Error = map[string]any{"code": "WORKER_START_FAILED", "message": err.Error()}
		execution.UpdatedAt = r.now()
		_ = r.store.SaveSkillExecution(execution)
		r.releaseRobot(request.RobotID, executionID)
		return execution, err
	}
	active := &activeSkill{definition: definition, worker: worker, cancel: cancel, actions: make(map[string]string)}
	r.mu.Lock()
	r.active[executionID] = active
	r.mu.Unlock()
	execution.Status = SkillRunning
	execution.UpdatedAt = r.now()
	if err := r.store.SaveSkillExecution(execution); err != nil {
		_ = worker.Kill()
		cancel()
		r.mu.Lock()
		delete(r.active, executionID)
		r.mu.Unlock()
		r.releaseRobot(request.RobotID, executionID)
		return SkillExecution{}, err
	}
	r.events.Report(execution, "skill.started", map[string]any{"status": execution.Status})
	go r.runWorker(workerContext, executionID, active)
	return cloneSkillExecution(execution), nil
}

func (r *SkillRuntime) Get(executionID string) (SkillExecution, error) {
	return r.store.GetSkillExecution(executionID)
}

func (r *SkillRuntime) Wait(ctx context.Context, executionID string) (SkillExecution, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		execution, err := r.store.GetSkillExecution(executionID)
		if err != nil {
			return SkillExecution{}, err
		}
		if skillTerminal(execution.Status) {
			return execution, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return execution, ctx.Err()
		}
	}
}

func (r *SkillRuntime) Stop(ctx context.Context, executionID, source, reason, mode string) (SkillExecution, error) {
	execution, err := r.store.GetSkillExecution(executionID)
	if err != nil {
		return SkillExecution{}, err
	}
	if skillTerminal(execution.Status) && execution.Status != SkillInterrupted {
		return execution, nil
	}
	active := r.getActive(executionID)
	if active != nil {
		// Server 重试、用户重复点击和 Pilot 退出可能同时请求停止。同一
		// Execution 只能有一个调用推进 Ability stop 与 Worker on_stop；
		// 后到调用等待后直接复用已经持久化的物理停止证据。
		active.stopMu.Lock()
		defer active.stopMu.Unlock()
		execution, err = r.store.GetSkillExecution(executionID)
		if err != nil {
			return SkillExecution{}, err
		}
		if skillTerminal(execution.Status) && execution.Status != SkillInterrupted {
			return execution, nil
		}
	}
	if active == nil {
		execution.Status = SkillInterrupted
		execution.Error = map[string]any{"code": "WORKER_NOT_FOUND", "message": "无法确认 Worker 和当前物理动作状态"}
		execution.UpdatedAt = r.now()
		_ = r.store.SaveSkillExecution(execution)
		return execution, ErrManualCheckRequired
	}
	active.stopDecisions()
	execution.Status = SkillStopping
	execution.UpdatedAt = r.now()
	_ = r.store.SaveSkillExecution(execution)
	// waiting_agent 不是物理终态。若不先解除 AgentRequest，Python Worker 会一直
	// 阻塞在 agent.request，无法读取随后发送的 skill.stop；最终 Server 只能长期
	// 停在 stopping。取消只负责唤醒当前 Worker，真正的 stopped 仍必须由下面的
	// Ability stop、Skill on_stop 和 Robot hold 证据共同决定。
	if canceler, ok := r.agent.(interface{ CancelExecution(string) }); ok {
		canceler.CancelExecution(executionID)
	}

	// 先停止当前 Ability；只有它释放物理资源后，Skill 的 stop entrypoint 才能执行 hold Action。
	active.mu.Lock()
	current := make([]string, 0, len(active.actions))
	for _, actionID := range active.actions {
		current = append(current, actionID)
	}
	active.mu.Unlock()
	worker := active.currentWorker()
	for _, actionID := range current {
		if _, stopErr := r.runner.Stop(ctx, actionID, reason); stopErr != nil {
			execution.Status = SkillInterrupted
			execution.Error = map[string]any{"code": "ABILITY_STOP_UNCONFIRMED", "message": stopErr.Error()}
			execution.UpdatedAt = r.now()
			_ = r.store.SaveSkillExecution(execution)
			_ = worker.Kill()
			active.cancel()
			return execution, stopErr
		}
	}

	outcome, err := worker.Stop(ctx, map[string]any{
		"id": "stop-" + uuid.NewString(), "source": source, "reason": reason, "mode": mode,
		"requested_at": r.now().UTC().Format(time.RFC3339Nano),
	})
	// Worker 会先上报 stop.outcome，再返回 skill.stop 的 RPC 回执。进程在这两条
	// 消息之间退出时，物理停止证据已经到达 Pilot，不能只因传输回执丢失就把
	// Robot 降为状态未知；反之，没有明确 safe 证据时仍必须保持 interrupted。
	// 重新读取 Store 也让 HTTP 重试、实例退出等并发停止复用同一份停止结果。
	if err != nil {
		persisted, loadErr := r.store.GetSkillExecution(executionID)
		if loadErr == nil {
			if safe, _ := persisted.StopOutcome["safe"].(bool); safe {
				outcome = cloneMap(persisted.StopOutcome)
				err = nil
			}
		}
	}
	if err != nil {
		_ = worker.Kill()
		active.cancel()
		execution.Status = SkillInterrupted
		execution.Error = map[string]any{"code": "WORKER_STOP_UNCONFIRMED", "message": err.Error()}
	} else if safe, _ := outcome["safe"].(bool); safe {
		execution.Status = SkillStopped
		execution.StopOutcome = cloneMap(outcome)
	} else {
		execution.Status = SkillInterrupted
		execution.StopOutcome = cloneMap(outcome)
		execution.Error = map[string]any{"code": "PHYSICAL_STOP_UNCONFIRMED", "message": "Skill 未形成安全停止证据"}
		_ = worker.Kill()
		active.cancel()
	}
	execution.UpdatedAt = r.now()
	_ = r.store.SaveSkillExecution(execution)
	r.events.Report(execution, "skill.stop.finalized", map[string]any{
		"status":        execution.Status,
		"error_code":    stringValue(execution.Error["code"]),
		"error_message": stringValue(execution.Error["message"]),
		"stop_outcome":  cloneMap(execution.StopOutcome),
	})
	if execution.Status == SkillStopped {
		r.cleanup(executionID, execution.RobotID)
	}
	return execution, err
}

// Recover 重建尚未结束的 Worker。Action key 会命中 Journal，只查询已有 invocation。
func (r *SkillRuntime) Recover(ctx context.Context) error {
	executions, err := r.store.ListRecoverableSkillExecutions()
	if err != nil {
		return err
	}
	for _, execution := range executions {
		definition, resolveErr := r.skills.Resolve(execution.SkillName, execution.SkillVersion)
		if resolveErr != nil {
			r.interruptSkill(execution.ID, "SKILL_UNAVAILABLE", resolveErr.Error())
			continue
		}
		r.mu.Lock()
		if owner := r.activeRobots[execution.RobotID]; owner != "" && owner != execution.ID {
			r.mu.Unlock()
			r.interruptSkill(execution.ID, "ROBOT_LOCK_CONFLICT", "Robot 已被另一 Execution 占用")
			continue
		}
		r.activeRobots[execution.RobotID] = execution.ID
		r.mu.Unlock()
		workerContext, cancel := context.WithCancel(context.Background())
		worker, startErr := r.supervisor.Start(workerContext, definition)
		if startErr != nil {
			cancel()
			r.interruptSkill(execution.ID, "WORKER_RECOVERY_FAILED", startErr.Error())
			continue
		}
		active := &activeSkill{definition: definition, worker: worker, cancel: cancel, actions: make(map[string]string)}
		r.mu.Lock()
		r.active[execution.ID] = active
		r.mu.Unlock()
		execution.Status = SkillRunning
		execution.UpdatedAt = r.now()
		_ = r.store.SaveSkillExecution(execution)
		go r.runWorker(workerContext, execution.ID, active)
	}
	return nil
}

func (r *SkillRuntime) runWorker(ctx context.Context, executionID string, active *activeSkill) {
	execution, err := r.store.GetSkillExecution(executionID)
	if err != nil {
		return
	}
	params := map[string]any{
		"execution_id":     execution.ID,
		"robot_ref":        execution.RobotID,
		"input":            execution.Input,
		"checkpoint":       execution.Checkpoint,
		"feedback_cursors": execution.FeedbackCursors,
		"log_fields": map[string]any{
			"robot_id": execution.RobotID, "project_id": execution.ProjectID,
			"task_id": execution.TaskID, "subtask_id": execution.SubtaskID,
			"skill_execution_id": execution.ID, "skill_name": execution.SkillName,
			"skill_version": execution.SkillVersion,
		},
	}
	if workspaces, ok := r.artifacts.(ArtifactWorkspace); ok {
		workspace, workspaceErr := workspaces.Workspace(execution)
		if workspaceErr != nil {
			r.interruptSkill(executionID, "ARTIFACT_WORKSPACE_FAILED", workspaceErr.Error())
			return
		}
		params["workspace"] = workspace
	}
	worker := active.currentWorker()
	result, runErr := worker.Run(ctx, params, func(callCtx context.Context, method string, arguments map[string]any) (any, error) {
		return r.handleWorker(callCtx, executionID, active, method, arguments)
	})
	if runErr != nil {
		latest, latestErr := r.store.GetSkillExecution(executionID)
		if latestErr == nil && (latest.Status == SkillStopping || latest.Status == SkillInterrupted || skillTerminal(latest.Status)) {
			return
		}
		if r.restartWorker(ctx, executionID, active, runErr.Error()) {
			return
		}
		if !active.hasStartedPhysicalAction() {
			r.failBeforePhysicalAction(executionID, "WORKER_EXITED", runErr.Error())
			return
		}
		r.interruptSkill(executionID, "WORKER_EXITED", runErr.Error())
		return
	}
	// 正常结束与 Stop 必须在同一个临界区内决定唯一终态。否则 skill.run
	// 刚返回时，普通清理路径可能与 on_stop 并发：前者先终止 Worker，后者
	// 虽然已经取得 Robot hold 证据，却只能收到进程被杀错误并把安全停止误报为未知。
	// 持锁直到终态持久化和清理完成，可保证先进入的路径完成原子决策；后进入
	// 的 Stop 或正常结束路径只复用已保存的终态，不重复发送物理命令。
	active.stopMu.Lock()
	defer active.stopMu.Unlock()
	execution, err = r.store.GetSkillExecution(executionID)
	if err != nil {
		return
	}
	// Stop 已锁存后，普通 skill.run 可能先返回 stopping/failed。此时只有
	// Stop 路径可以写入终态和清理 Worker，否则会在 on_stop 返回证据前杀掉进程。
	if execution.Status == SkillStopping || skillTerminal(execution.Status) {
		return
	}
	status, _ := result["status"].(string)
	// Worker 返回 failed 不等于设备动作已结束。保留未知动作的 Skill 和
	// Robot 占用，让 Server / Web 继续展示原执行并允许停止，而非接纳新任务。
	if owner := r.runner.physicalOwner(execution.RobotID); owner != "" {
		if action, loadErr := r.runner.journal.GetActionByID(owner); loadErr == nil && action.SkillExecutionID == executionID {
			r.interruptSkill(executionID, "PHYSICAL_ACTION_UNCONFIRMED", "物理动作尚未确认结束，请停止当前执行后再启动新任务")
			return
		}
	}
	switch status {
	case "completed":
		execution.Status = SkillCompleted
		execution.Result, _ = result["result"].(map[string]any)
	case "failed":
		execution.Status = SkillFailed
		execution.Error, _ = result["error"].(map[string]any)
		// failed 不再丢弃 result：Ability 的安全保持证据（safe/hold）就在这里，
		// Workflow 靠它区分“失败但已安全保持”和“物理状态未知”。
		if value, ok := result["result"].(map[string]any); ok {
			execution.Result = cloneMap(value)
		}
	case "stopping":
		return
	default:
		execution.Status = SkillInterrupted
		execution.Error = map[string]any{"code": "WORKER_TERMINAL_INVALID", "message": fmt.Sprintf("unexpected status %q", status)}
	}
	execution.UpdatedAt = r.now()
	_ = r.store.SaveSkillExecution(execution)
	if execution.Status == SkillCompleted {
		artifact, artifactErr := r.publishExecutionSummary(ctx, execution)
		if artifactErr != nil {
			r.events.Report(execution, "artifact.summary.failed", map[string]any{"error": artifactErr.Error()})
		} else {
			r.events.Report(execution, "artifact.summary.announced", artifact)
		}
	}
	r.events.Report(execution, "execution.terminal", map[string]any{
		"status": execution.Status, "result": cloneMap(execution.Result),
		"error": cloneMap(execution.Error),
	})
	if execution.Status != SkillInterrupted {
		r.cleanup(executionID, execution.RobotID)
	}
}

// publishExecutionSummary 生成体积很小的语义执行报告，用来验证并承载
// Pilot→Server Artifact 正式链路。它不复制图像或原始传感流，也不会因
// 同步失败改变已经确认的 Robot 执行终态；失败会作为独立事件显示在 Studio。
func (r *SkillRuntime) publishExecutionSummary(ctx context.Context, execution SkillExecution) (map[string]any, error) {
	workspaces, ok := r.artifacts.(ArtifactWorkspace)
	if !ok || r.artifacts == nil {
		return nil, errors.New("artifact workspace is unavailable")
	}
	workspace, err := workspaces.Workspace(execution)
	if err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(map[string]any{
		"execution_id": execution.ID, "robot_id": execution.RobotID,
		"skill_name": execution.SkillName, "skill_version": execution.SkillVersion,
		"status": execution.Status, "result": cloneMap(execution.Result),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(workspace, "execution-summary.json")
	if err := os.WriteFile(path, append(body, byte(10)), 0o600); err != nil {
		return nil, err
	}
	return r.artifacts.Publish(ctx, execution, path, "application/json", "Robot Skill 执行结果摘要")
}

// restartWorker 只重启 Python 解释器。恢复后的相同 Action key 会查询 Journal 中的 invocation，
// 因此 Worker 崩溃不会再次发送物理命令。
func (r *SkillRuntime) restartWorker(ctx context.Context, executionID string, active *activeSkill, workerError string) bool {
	active.mu.Lock()
	if active.restarts >= 1 {
		active.mu.Unlock()
		return false
	}
	active.restarts++
	active.mu.Unlock()
	worker, err := r.supervisor.Start(ctx, active.definition)
	if err != nil {
		return false
	}
	active.replaceWorker(worker)
	r.events.Report(SkillExecution{ID: executionID}, "worker.restarted", map[string]any{"reason": "worker_exit", "error": workerError})
	go r.runWorker(ctx, executionID, active)
	return true
}

func (r *SkillRuntime) handleWorker(ctx context.Context, executionID string, active *activeSkill, method string, params map[string]any) (any, error) {
	execution, err := r.store.GetSkillExecution(executionID)
	if err != nil {
		return nil, err
	}
	switch method {
	case "checkpoint":
		execution.Checkpoint, _ = params["state"].(map[string]any)
		execution.UpdatedAt = r.now()
		return nil, r.store.SaveSkillExecution(execution)
	case "action.start":
		return r.startWorkerAction(ctx, execution, active, params)
	case "action.feedback":
		actionID, _ := params["action_id"].(string)
		after := int64(number(params["after_sequence"]))
		feedback, terminal, feedbackErr := r.runner.Feedback(ctx, actionID, after)
		if feedbackErr == nil && len(feedback) > 0 {
			action, actionErr := r.runner.journal.GetActionByID(actionID)
			if actionErr == nil {
				if execution.FeedbackCursors == nil {
					execution.FeedbackCursors = make(map[string]int64)
				}
				execution.FeedbackCursors[action.Key] = feedback[len(feedback)-1].Sequence
				execution.UpdatedAt = r.now()
				_ = r.store.SaveSkillExecution(execution)
				for _, item := range feedback {
					r.events.Report(execution, "feedback.emitted", withExecutionStage(execution, map[string]any{
						"action_id": action.ID, "action_type": action.Action.Type, "feedback": item,
					}))
				}
			}
		}
		return map[string]any{"feedback": feedback, "terminal": terminal}, feedbackErr
	case "action.result":
		actionID, _ := params["action_id"].(string)
		return r.waitActionResult(ctx, executionID, active, actionID)
	case "action.stop":
		actionID, _ := params["action_id"].(string)
		reason, _ := params["reason"].(string)
		stopped, stopErr := r.runner.Stop(ctx, actionID, reason)
		return actionResultPayload(stopped), stopErr
	case "observation.get":
		if r.observations == nil {
			return nil, nil
		}
		ref, _ := params["observation_ref"].(string)
		return r.observations.Get(ctx, execution.RobotID, ref)
	case "observation.latest":
		if r.observations == nil {
			return nil, nil
		}
		kind, _ := params["kind"].(string)
		subject, _ := params["subject_ref"].(string)
		maxAge := time.Duration(number(params["max_age_ms"])) * time.Millisecond
		return r.observations.Latest(ctx, execution.RobotID, kind, subject, maxAge)
	case "agent.request":
		return r.requestAgent(ctx, execution, active, params)
	case "artifact.resolve":
		if r.artifacts == nil {
			return nil, fmt.Errorf("artifact service is unavailable")
		}
		ref, _ := params["ref"].(string)
		return r.artifacts.Resolve(ctx, execution, ref)
	case "artifact.publish":
		if r.artifacts == nil {
			return nil, fmt.Errorf("artifact service is unavailable")
		}
		path, _ := params["path"].(string)
		mediaType, _ := params["media_type"].(string)
		summary, _ := params["summary"].(string)
		return r.artifacts.Publish(ctx, execution, path, mediaType, summary)
	case "runtime.now":
		return r.now().UTC().Format(time.RFC3339Nano), nil
	case "event.report":
		event, _ := params["event"].(string)
		// Worker 会先上报 stage.running，再发起该阶段的第一个 Action。这里把
		// Skill 明确声明的当前 Stage 写入恢复 checkpoint，后续 Action、Feedback
		// 和 Observation 才能从源头携带正确归属；Pilot 不根据 Action 类型猜 Stage。
		if event == "stage.running" {
			if stage, ok := params["stage"].(string); ok && stage != "" {
				if execution.Checkpoint == nil {
					execution.Checkpoint = make(map[string]any)
				} else {
					execution.Checkpoint = cloneMap(execution.Checkpoint)
				}
				execution.Checkpoint["stage"] = stage
				execution.UpdatedAt = r.now()
				if err := r.store.SaveSkillExecution(execution); err != nil {
					return nil, err
				}
			}
		}
		r.events.Report(execution, event, cloneMap(params))
		return nil, nil
	case "log":
		level, _ := params["level"].(string)
		message, _ := params["message"].(string)
		r.events.Log(execution, level, message, cloneMap(params))
		return nil, nil
	case "complete", "fail":
		// complete/fail 是 Worker 进程内的返回通知。此时 SkillExecution 尚未由
		// runWorker 收敛，向 Server 转发会携带旧 running 状态并覆盖正式终态。
		return nil, nil
	case "stop.outcome":
		r.events.Report(execution, method, cloneMap(params))
		_ = r.store.UpdateSkillStopOutcome(execution.ID, params)
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported worker method %s", method)
	}
}

func (r *SkillRuntime) startWorkerAction(ctx context.Context, execution SkillExecution, active *activeSkill, params map[string]any) (any, error) {
	key, _ := params["key"].(string)
	actionValue, _ := params["action"].(map[string]any)
	action := ActionRef{Type: stringValue(actionValue["type"]), SchemaVersion: int(number(actionValue["schema_version"]))}
	stopAction, _ := params["stop_action"].(bool)
	if !actionDeclared(active.definition, action, stopAction) {
		return nil, fmt.Errorf("action %s is not declared by skill", action.Key())
	}
	input, _ := actionValue["parameters"].(map[string]any)
	timeoutSeconds := number(actionValue["timeout_seconds"])
	feedbackValue, _ := actionValue["feedback"].(map[string]any)
	feedbackInterval := time.Duration(number(feedbackValue["interval_ms"])) * time.Millisecond
	started, err := r.runner.StartAction(ctx, ActionRequest{
		SkillExecutionID: execution.ID, Key: key, RobotID: execution.RobotID,
		Action: action, Input: input, Timeout: time.Duration(timeoutSeconds * float64(time.Second)),
		FeedbackInterval: feedbackInterval, StopAction: stopAction,
	})
	startRejected := started.Status == ActionFailed &&
		stringValue(started.Error["code"]) == "ABILITY_START_REJECTED"
	if started.Physical && !startRejected {
		active.mu.Lock()
		active.physicalActionStarted = true
		active.mu.Unlock()
	}
	// StartTask 丢失回执时 Runner 仍保存了 Action 和 invocation。必须先关联
	// 再返回错误，否则 Skill 的停止路径找不到这个仍持有物理锁的 Action。
	if started.ID != "" {
		active.mu.Lock()
		active.actions[key] = started.ID
		active.mu.Unlock()
	}
	if err != nil && !errors.Is(err, ErrReplayForbidden) {
		return nil, err
	}
	active.mu.Lock()
	active.actions[key] = started.ID
	active.mu.Unlock()
	r.events.Report(execution, "action.started", withExecutionStage(execution, map[string]any{
		"action_key":          key,
		"action_type":         action.Type,
		"action_id":           started.ID,
		"ability_name":        started.AbilityName,
		"ability_task":        started.TaskName,
		"ability_instance_id": started.InstanceID,
		"invocation_id":       started.InvocationID,
		"framework_task_id":   started.FrameworkTaskID,
		"status":              started.Status,
		"physical":            started.Physical,
		"physical_started":    started.Physical && !startRejected,
	}))
	return map[string]any{"action_id": started.ID, "status": started.Status}, nil
}

func (a *activeSkill) hasStartedPhysicalAction() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.physicalActionStarted
}

func (r *SkillRuntime) waitActionResult(ctx context.Context, executionID string, active *activeSkill, actionID string) (map[string]any, error) {
	// Ability在自己的进程内持续执行安全监视，Pilot这里只负责收敛状态。
	// 20ms轮询会让一次携物动作产生数千次HTTP/SQLite读取，并直接拖慢同机
	// MuJoCo物理循环。沿用Skill声明的反馈周期，普通状态最多约5Hz；错误、
	// 阶段变化和终态仍在下一次状态读取时立即保存。
	ticker := time.NewTicker(r.runner.actionPollInterval(actionID))
	defer ticker.Stop()
	for {
		execution, err := r.runner.Result(ctx, actionID)
		if err != nil && execution.ID == "" {
			return nil, err
		}
		skillExecution, skillErr := r.store.GetSkillExecution(executionID)
		if skillErr != nil {
			skillExecution = SkillExecution{ID: executionID}
		}
		// Feedback 是执行中的观测，不应等 Action 终态后再整批刷给 Server。
		// 按 Runner 已限频的游标增量上报，既让 Studio 能看到真实进度，也
		// 避免 Worker 结束时集中写入大量重复事件拖慢终态收敛。
		r.reportActionFeedback(skillExecution, execution)
		if actionTerminal(execution.Status) {
			active.mu.Lock()
			for key, id := range active.actions {
				if id == actionID && execution.Status != ActionInterrupted {
					delete(active.actions, key)
				}
			}
			active.mu.Unlock()
			execution = r.importAbilityArtifacts(ctx, skillExecution, execution)
			// Worker 可以选择流式读取 Feedback，也可以直接等待 Result。这里从
			// 已持久化游标补发尚未上报的 Feedback，并把最终 Observation 独立
			// 记录到 Server，保证 Studio 不必从 Action Result 中猜测证据。
			r.reportActionFeedback(skillExecution, execution)
			for _, observation := range execution.Observations {
				r.events.Report(skillExecution, "observation.recorded", withExecutionStage(skillExecution, map[string]any{
					"action_id": execution.ID, "action_type": execution.Action.Type, "observation": observation,
				}))
			}
			r.events.Report(skillExecution, "action.terminal", withExecutionStage(skillExecution, map[string]any{
				"action_key":          execution.Key,
				"action_type":         execution.Action.Type,
				"action_id":           execution.ID,
				"ability_name":        execution.AbilityName,
				"ability_task":        execution.TaskName,
				"ability_instance_id": execution.InstanceID,
				"invocation_id":       execution.InvocationID,
				"framework_task_id":   execution.FrameworkTaskID,
				"status":              execution.Status,
				"result":              cloneMap(execution.Result),
				"error":               cloneMap(execution.Error),
			}))
			return actionResultPayload(execution), nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *SkillRuntime) reportActionFeedback(skillExecution SkillExecution, execution ActionExecution) {
	if len(execution.Feedback) == 0 || execution.Key == "" {
		return
	}
	if skillExecution.FeedbackCursors == nil {
		skillExecution.FeedbackCursors = make(map[string]int64)
	}
	reportedCursor := skillExecution.FeedbackCursors[execution.Key]
	latestCursor := reportedCursor
	for _, item := range execution.Feedback {
		if item.Sequence <= reportedCursor {
			continue
		}
		r.events.Report(skillExecution, "feedback.emitted", withExecutionStage(skillExecution, map[string]any{
			"action_id": execution.ID, "action_type": execution.Action.Type, "feedback": item,
		}))
		if item.Sequence > latestCursor {
			latestCursor = item.Sequence
		}
	}
	if latestCursor == reportedCursor {
		return
	}
	skillExecution.FeedbackCursors[execution.Key] = latestCursor
	skillExecution.UpdatedAt = r.now()
	_ = r.store.SaveSkillExecution(skillExecution)
}

func (r *SkillRuntime) importAbilityArtifacts(ctx context.Context, skillExecution SkillExecution, execution ActionExecution) ActionExecution {
	importer, ok := r.artifacts.(AbilityArtifactImporter)
	if !ok || execution.Result == nil {
		return execution
	}
	rawCandidates, ok := execution.Result["artifact_candidates"].([]any)
	if !ok || len(rawCandidates) == 0 {
		return execution
	}
	result := cloneMap(execution.Result)
	candidates := make([]any, 0, len(rawCandidates))
	artifactRefs := make([]any, 0, len(rawCandidates))
	publicationErrors := make([]any, 0)
	for _, raw := range rawCandidates {
		candidate, ok := raw.(map[string]any)
		if !ok {
			candidates = append(candidates, raw)
			continue
		}
		item := cloneMap(candidate)
		published, err := importer.ImportAbilityArtifact(
			ctx,
			skillExecution,
			stringValue(item["exchange_path"]),
			stringValue(item["media_type"]),
			"Robot Skill 关键检查点传感器证据",
		)
		if err != nil {
			item["publication_state"] = "failed"
			item["publication_error"] = err.Error()
			publicationErrors = append(publicationErrors, map[string]any{
				"candidate_id": item["candidate_id"],
				"message":      err.Error(),
			})
		} else {
			item["publication_state"] = stringValue(published["sync_status"])
			item["local_ref"] = published["local_ref"]
			if ref := stringValue(published["local_ref"]); ref != "" {
				artifactRefs = append(artifactRefs, ref)
			}
		}
		candidates = append(candidates, item)
	}
	result["artifact_candidates"] = candidates
	result["artifact_refs"] = artifactRefs
	if len(publicationErrors) > 0 {
		result["artifact_publication_errors"] = publicationErrors
	}
	execution.Result = result
	if len(artifactRefs) > 0 {
		observations := cloneMaps(execution.Observations)
		for index := range observations {
			if stringValue(observations[index]["kind"]) == "sensor.frame" {
				observations[index]["evidence_refs"] = artifactRefs
			}
		}
		execution.Observations = observations
	}
	_ = r.runner.journal.SaveAction(execution)
	return execution
}

// withExecutionStage 把最近 checkpoint 中由 Skill 明确上报的 Stage 附到
// Action、Feedback 和 Observation 上。Pilot 不推测 Python 内部流程；若 Skill
// 尚未 checkpoint，则保持字段为空，前端也不会伪造归属。
func withExecutionStage(execution SkillExecution, fields map[string]any) map[string]any {
	result := cloneMap(fields)
	if execution.Checkpoint == nil {
		return result
	}
	if stage, ok := execution.Checkpoint["stage"].(string); ok && stage != "" {
		result["stage"] = stage
	}
	return result
}

func (r *SkillRuntime) requestAgent(ctx context.Context, execution SkillExecution, active *activeSkill, params map[string]any) (any, error) {
	if r.agent == nil {
		return nil, fmt.Errorf("agent gateway is unavailable")
	}
	// Stop must cancel both an existing wait and a request arriving after the
	// active action returns stopped. Keep the Worker RPC loop free for on_stop.
	active.mu.Lock()
	if active.stopping {
		active.mu.Unlock()
		return nil, context.Canceled
	}
	ctx, cancel := context.WithCancel(ctx)
	active.decisionCancel = cancel
	active.mu.Unlock()
	defer cancel()
	key, _ := params["key"].(string)
	contextValue, _ := params["context"].(map[string]any)
	responseSchema, _ := params["response_schema"].(map[string]any)
	revision := int(number(contextValue["decision_revision"]))
	if revision == 0 {
		if state, ok := contextValue["state"].(map[string]any); ok {
			revision = int(number(state["decision_revision"]))
		}
	}
	if revision == 0 {
		revision = int(number(contextValue["plan_revision"]))
	}
	request := AgentRequest{
		ExecutionID: execution.ID, SkillName: execution.SkillName, Stage: stringValue(contextValue["stage"]),
		DecisionKey: key, DecisionRevision: revision,
		Reason: stringValue(params["reason"]), Context: cloneMap(contextValue),
		ResponseModel:  stringValue(params["response_model"]),
		ResponseSchema: cloneMap(responseSchema), CreatedAt: r.now(),
	}
	active.mu.Lock()
	if active.stopping {
		active.mu.Unlock()
		return nil, context.Canceled
	}
	if err := r.store.SaveAgentRequest(request); err != nil {
		active.mu.Unlock()
		return nil, err
	}
	// AgentGateway 会先登记等待句柄，再上报唯一一次 agent.requested。若 Runtime
	// 提前上报同一事件，Server 的快速回复可能先于等待句柄到达并被 Pilot 丢弃。
	execution.Status = SkillWaitingAgent
	execution.UpdatedAt = r.now()
	if err := r.store.SaveSkillExecution(execution); err != nil {
		active.mu.Unlock()
		return nil, err
	}
	active.mu.Unlock()

	reply, err := r.agent.Request(ctx, request)
	if err != nil {
		return nil, err
	}
	if reply.ExecutionID != request.ExecutionID || reply.SkillName != request.SkillName ||
		reply.Stage != request.Stage || reply.DecisionKey != request.DecisionKey ||
		reply.DecisionRevision != request.DecisionRevision {
		return nil, fmt.Errorf("agent reply does not match execution, stage or decision revision")
	}
	for _, forbidden := range []string{"python", "trajectory", "stage", "action_type"} {
		if _, exists := reply.Payload[forbidden]; exists {
			return nil, fmt.Errorf("agent reply contains forbidden field %s", forbidden)
		}
	}
	active.mu.Lock()
	defer active.mu.Unlock()
	if active.stopping {
		return nil, context.Canceled
	}
	resolvedAt := r.now()
	request.Response = cloneMap(reply.Payload)
	request.ResolvedAt = &resolvedAt
	if err := r.store.SaveAgentRequest(request); err != nil {
		return nil, err
	}
	execution.Status = SkillRunning
	r.events.Report(execution, "agent.resolved", map[string]any{
		"decision_key": key,
		"stage":        request.Stage,
	})
	execution.UpdatedAt = resolvedAt
	_ = r.store.SaveSkillExecution(execution)
	return reply.Payload, nil
}

func actionDeclared(definition SkillDefinition, action ActionRef, stop bool) bool {
	items := definition.RequiredActions
	if stop {
		items = definition.StopActions
	}
	for _, item := range items {
		if item == action {
			return true
		}
	}
	return false
}

func actionResultPayload(execution ActionExecution) map[string]any {
	observations := execution.Observations
	if observations == nil {
		observations = []map[string]any{}
	}
	payload := map[string]any{
		"status": string(execution.Status), "output": execution.Result,
		"observations": observations, "evidence_refs": []string{},
		"physical_effect": "unknown",
	}
	if execution.Status == ActionSucceeded {
		payload["physical_effect"] = "confirmed"
	}
	if execution.Status == ActionFailed {
		payload["physical_effect"] = "none"
	}
	if execution.Status == ActionStopped {
		payload["physical_effect"] = "confirmed"
	}
	if code, ok := execution.Error["code"]; ok {
		payload["error_code"] = code
	}
	if message, ok := execution.Error["message"]; ok {
		payload["error_message"] = message
	}
	if refs, ok := execution.Result["evidence_refs"]; ok {
		payload["evidence_refs"] = refs
	}
	return payload
}

func (r *SkillRuntime) interruptSkill(executionID, code, message string) {
	execution, err := r.store.GetSkillExecution(executionID)
	if err != nil {
		return
	}
	if skillTerminal(execution.Status) {
		return
	}
	execution.Status = SkillInterrupted
	execution.Error = map[string]any{"code": code, "message": message}
	execution.UpdatedAt = r.now()
	if err := r.store.SaveSkillExecution(execution); err != nil {
		return
	}
	// Worker 启动或运行失败时，Pilot 过去只修改本地 SQLite，Server 端的
	// Robot Execution 会永久停留在 running。interrupted 虽然不能释放 Robot
	// 物理锁，也同样是必须立刻对外发布的终态；Server 收到后才能暂停 SubTask，
	// 展示真实错误并等待对账，而不是超时后猜测设备状态。
	r.events.Report(execution, "execution.terminal", map[string]any{
		"status": execution.Status, "result": cloneMap(execution.Result),
		"error": cloneMap(execution.Error),
	})
	// interrupted 不清除 activeRobots；必须经过人工或设备侧确认。
}

// failBeforePhysicalAction 收敛 Worker 的输入、模型或启动阶段错误。这里没有
// Action 到达设备，不存在需要人工确认的物理状态；使用 failed 也让用户修正
// 参数后可以立即再次调试，而不必重启整个 Robot 实例。
func (r *SkillRuntime) failBeforePhysicalAction(executionID, code, message string) {
	execution, err := r.store.GetSkillExecution(executionID)
	if err != nil || skillTerminal(execution.Status) {
		return
	}
	execution.Status = SkillFailed
	execution.Error = map[string]any{"code": code, "message": message}
	execution.UpdatedAt = r.now()
	if err := r.store.SaveSkillExecution(execution); err != nil {
		return
	}
	r.events.Report(execution, "execution.terminal", map[string]any{
		"status": execution.Status, "result": cloneMap(execution.Result),
		"error": cloneMap(execution.Error),
	})
	r.cleanup(executionID, execution.RobotID)
}

func (r *SkillRuntime) cleanup(executionID, robotID string) {
	r.mu.Lock()
	active := r.active[executionID]
	delete(r.active, executionID)
	if r.activeRobots[robotID] == executionID {
		delete(r.activeRobots, robotID)
	}
	r.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

func (r *SkillRuntime) releaseRobot(robotID, executionID string) {
	r.mu.Lock()
	if r.activeRobots[robotID] == executionID {
		delete(r.activeRobots, robotID)
	}
	r.mu.Unlock()
}

func (r *SkillRuntime) getActive(executionID string) *activeSkill {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[executionID]
}

func number(value any) float64 {
	switch item := value.(type) {
	case float64:
		return item
	case int:
		return float64(item)
	case int64:
		return float64(item)
	default:
		return 0
	}
}
func stringValue(value any) string { result, _ := value.(string); return result }
