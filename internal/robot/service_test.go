package robot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

type recordedRobotEvent struct {
	ProjectID    string
	ResourceType string
	ResourceID   string
	Type         string
	Revision     int64
	Payload      any
}

type recordingRobotEvents struct{ items []recordedRobotEvent }

func (r *recordingRobotEvents) PublishRobotEvent(projectID, resourceType, resourceID, eventType string, revision int64, payload any) {
	r.items = append(r.items, recordedRobotEvent{projectID, resourceType, resourceID, eventType, revision, payload})
}

func openRobotTestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
}

func TestHeartbeatPublishesOnlyMeaningfulPilotChanges(t *testing.T) {
	st := openRobotTestStore(t)
	events := &recordingRobotEvents{}
	service := NewService(st, events)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	pilot := store.RobotPilot{
		PilotInstanceID: "pilot-heartbeat", RobotID: "robot-heartbeat",
		RobotModel: "r1_pro_chassis", Backend: "mujoco", PilotVersion: "0.5.0",
		RobotStatus: "idle", AbilityFrameworkStatus: "ready",
		Abilities: []map[string]any{{"instance_id": "ability-1", "status": "ready"}},
	}
	_, disconnect, err := service.Connect(pilot)
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	stored, err := st.GetRobotPilot(pilot.PilotInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	events.items = nil

	if err := service.Heartbeat(pilot.PilotInstanceID, pilot); err != nil {
		t.Fatal(err)
	}
	afterHeartbeat, err := st.GetRobotPilot(pilot.PilotInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events.items) != 0 {
		t.Fatalf("无状态变化的心跳不应发布完整Pilot事件: %#v", events.items)
	}
	if afterHeartbeat.Revision != stored.Revision {
		t.Fatalf("纯心跳不应增加资源revision: before=%d after=%d", stored.Revision, afterHeartbeat.Revision)
	}
	if !afterHeartbeat.LastSeenAt.After(stored.LastSeenAt) {
		t.Fatalf("纯心跳仍应更新last_seen_at: before=%s after=%s", stored.LastSeenAt, afterHeartbeat.LastSeenAt)
	}

	pilot.RobotStatus = "busy"
	if err := service.Heartbeat(pilot.PilotInstanceID, pilot); err != nil {
		t.Fatal(err)
	}
	if len(events.items) != 1 || events.items[0].Type != "pilot.status" {
		t.Fatalf("真实状态变化必须发布一次pilot.status: %#v", events.items)
	}
	changed, err := st.GetRobotPilot(pilot.PilotInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision != stored.Revision+1 {
		t.Fatalf("真实状态变化必须增加一次revision: before=%d after=%d", stored.Revision, changed.Revision)
	}
}

func runWithValidSkillInput(t *testing.T, service *Service, commands <-chan PilotCommand,
	pilotID string, request RunRequest) (store.RobotExecution, error) {
	t.Helper()
	type outcome struct {
		execution store.RobotExecution
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		execution, err := service.Run(context.Background(), request)
		done <- outcome{execution: execution, err: err}
	}()
	command := receiveCommand(t, commands, "skill.validate_input")
	if err := service.HandleCommandAckResult(pilotID, command.CommandID, true, "",
		map[string]any{"valid": true, "input": request.Input}); err != nil {
		t.Fatal(err)
	}
	result := <-done
	return result.execution, result.err
}

func TestValidateTaskSkillScopeUsesApprovedSubTask(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-scope", "scope")
	if err != nil {
		t.Fatal(err)
	}
	conversation := store.ChatSession{ID: "conversation-scope", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "scope", CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC()}
	if err := st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	draft := store.WorkflowDraft{
		Goal: "搬运一个箱子",
		Tasks: []store.TaskDraft{{ID: "task-scope", RequiredRole: "robot", Goal: "由一个 Robot 完成搬运",
			SubTasks: []store.SubTaskDraft{{ID: "subtask-scope", Kind: "robot_skill",
				Goal: "抓取目标", Spec: json.RawMessage(
					`{"skill_name":"grasp-object","skill_version":"0.1.0"}`)}}}},
	}
	ready, err := st.SubmitPlanProposal(project.ID, conversation.ID, draft, "将目标箱体搬运到指定位置", json.RawMessage(`{"robot_ids":["r1pro-1"],"allowed_skills":["grasp-object"]}`),
		"# 搬运一个箱子", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(ready.ID, ready.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:r1pro-1", "r1pro-1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	task, err = st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning,
		"", "", nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	subtask, _ := st.GetSubTask("subtask-scope")
	if _, err = st.TransitionSubTask(subtask.ID, subtask.Revision,
		store.TaskStatusRunning, "", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	request := RunRequest{ProjectID: project.ID, WorkflowID: view.Workflow.ID,
		TaskID: task.ID, SubtaskID: subtask.ID, RobotID: "r1pro-1",
		SkillName: "grasp-object", SkillVersion: "0.1.0"}
	if err := service.ValidateTaskSkillScope(request); err != nil {
		t.Fatalf("批准范围内的 Skill 被拒绝: %v", err)
	}
	request.SkillName = "place-object"
	if !errors.Is(service.ValidateTaskSkillScope(request), ErrSkillOutsideTask) {
		t.Fatal("当前 SubTask 不能借 Robot Task 运行另一个 Skill")
	}
}

func receiveCommand(t *testing.T, commands <-chan PilotCommand, expected string) PilotCommand {
	t.Helper()
	select {
	case command := <-commands:
		if command.Type != expected {
			t.Fatalf("command type=%s want=%s", command.Type, expected)
		}
		return command
	case <-time.After(time.Second):
		t.Fatalf("未收到 %s", expected)
		return PilotCommand{}
	}
}

func TestRunRejectsInvalidSkillInputBeforeCreatingExecution(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-input", "input validation")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-input", RobotID: "robot-input", RobotStatus: "idle",
		AbilityFrameworkStatus: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "pilot-input",
		Name: "grasp-object", Version: "0.4.0", Status: "installed", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := service.Run(context.Background(), RunRequest{ProjectID: project.ID,
			RobotID: "robot-input", SkillName: "grasp-object", SkillVersion: "0.4.0",
			RequestKey: "invalid-input", Input: map[string]any{"target": map[string]any{}}})
		done <- runErr
	}()
	command := receiveCommand(t, commands, "skill.validate_input")
	if err := service.HandleCommandAckResult("pilot-input", command.CommandID, true, "",
		map[string]any{"valid": false, "error": "target.object_ref: Field required"}); err != nil {
		t.Fatal(err)
	}
	if runErr := <-done; !errors.Is(runErr, ErrSkillInputInvalid) {
		t.Fatalf("非法输入应返回可纠正诊断，err=%v", runErr)
	}
	executions, err := st.ListRobotExecutions(project.ID, "robot-input", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(executions) != 0 {
		t.Fatalf("输入预检失败不得创建Robot Execution: %+v", executions)
	}
	select {
	case command := <-commands:
		t.Fatalf("输入预检失败不得下发物理命令: %+v", command)
	default:
	}
}

func TestDeviceSnapshotUsesArraysAndCountsHealthyAbilities(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	_, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-empty", RobotID: "robot-empty", RobotStatus: "idle",
		Abilities: []map[string]any{
			{"instance_id": "ability-1", "healthy": true, "state": "Running"},
			{"instance_id": "ability-2", "healthy": false, "state": "Running"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()

	snapshot, err := service.DeviceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"skill_packages", "executions"} {
		if value, ok := document[field].([]any); !ok || len(value) != 0 {
			t.Fatalf("%s 必须是空数组，实际 %#v", field, document[field])
		}
	}
	robots, ok := document["robots"].([]any)
	if !ok || len(robots) != 1 {
		t.Fatalf("robots=%#v", document["robots"])
	}
	device := robots[0].(map[string]any)
	if skills, ok := device["installed_skills"].([]any); !ok || len(skills) != 0 {
		t.Fatalf("installed_skills 必须是空数组，实际 %#v", device["installed_skills"])
	}
	framework := device["ability_framework"].(map[string]any)
	if framework["healthy_instances"] != float64(1) || framework["total_instances"] != float64(2) {
		t.Fatalf("AbilityFramework 健康计数错误: %#v", framework)
	}
}

func TestConnectWithSkillSnapshotReplacesStaleActualCatalog(t *testing.T) {
	st := openRobotTestStore(t)
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: "pilot-reused", Name: "grasp-object", Version: "0.2.0",
		Enabled: true, Status: "installed", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	_, disconnect, err := service.ConnectWithSkillSnapshot(store.RobotPilot{
		PilotInstanceID: "pilot-reused", RobotID: "robot-new-instance", RobotStatus: "idle",
	}, []store.RobotPilotSkill{})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	actual, err := st.ListRobotPilotSkills("pilot-reused")
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != 0 {
		t.Fatalf("新实例的空 active 目录不得继承旧 actual: %#v", actual)
	}
}

func TestFailedExecutionStartAckTerminatesQueuedExecution(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-reject", "Reject start")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-reject", RobotID: "robot-reject", RobotStatus: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: "pilot-reject", Name: "grasp-object", Version: "0.2.0",
		Enabled: true, Status: "installed", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	execution, err := runWithValidSkillInput(t, service, commands, "pilot-reject", RunRequest{
		ProjectID: project.ID, RobotID: "robot-reject", SkillName: "grasp-object",
		SkillVersion: "0.2.0", RequestKey: "request-reject",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := receiveCommand(t, commands, "execution.start")
	if err := service.HandleCommandAck("pilot-reject", command.CommandID, false,
		"robot skill package is invalid: unknown skill grasp-object"); err == nil {
		t.Fatal("Pilot 拒绝启动必须向 Gateway 日志返回错误")
	}
	failed, err := st.GetRobotExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.Error["code"] != "PILOT_START_REJECTED" {
		t.Fatalf("拒绝启动后 Execution 没有收敛: %#v", failed)
	}
	pilot, err := st.GetRobotPilot("pilot-reject")
	if err != nil {
		t.Fatal(err)
	}
	if pilot.CurrentExecutionID != "" || pilot.RobotStatus != "idle" {
		t.Fatalf("拒绝启动后 Robot 仍被幽灵执行占用: %#v", pilot)
	}
}

func TestFailedExecutionStopAckLeavesInterruptedEvidence(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-stop-reject", "Reject stop")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-stop-reject", RobotID: "robot-stop-reject", RobotStatus: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: "pilot-stop-reject", Name: "grasp-object", Version: "0.2.0",
		Enabled: true, Status: "installed", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	execution, err := runWithValidSkillInput(t, service, commands, "pilot-stop-reject", RunRequest{
		ProjectID: project.ID, RobotID: "robot-stop-reject", SkillName: "grasp-object",
		SkillVersion: "0.2.0", RequestKey: "request-stop-reject",
	})
	if err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.start")
	if _, err := service.Stop(context.Background(), project.ID, execution.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	command := receiveCommand(t, commands, "execution.stop")
	if err := service.HandleCommandAck("pilot-stop-reject", command.CommandID, false,
		"pilot execution not found"); err == nil {
		t.Fatal("Pilot 拒绝 stop 必须向 Gateway 日志返回错误")
	}
	interrupted, err := st.GetRobotExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != "interrupted" || interrupted.Error["code"] != "PILOT_STOP_REJECTED" {
		t.Fatalf("stop 拒绝后不能停留在 stopping 或伪造 stopped: %#v", interrupted)
	}
}

func TestRunRejectsPersistedInterruptedRobotLock(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-interrupted-lock", "interrupted lock")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID:    "pilot-interrupted-lock",
		RobotID:            "robot-interrupted-lock",
		RobotStatus:        "interrupted",
		CurrentExecutionID: "rex-interrupted-lock",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()

	_, err = service.Run(context.Background(), RunRequest{
		ProjectID: project.ID, RobotID: "robot-interrupted-lock",
		SkillName: "grasp-object", SkillVersion: "0.2.0",
		RequestKey: "must-not-bypass-interrupted-lock",
	})
	if !errors.Is(err, ErrRobotBusy) {
		t.Fatalf("Pilot 尚未确认 hold 时必须拒绝新执行，err=%v", err)
	}
	select {
	case command := <-commands:
		t.Fatalf("拒绝的新执行不应向 Pilot 下发命令: %#v", command)
	default:
	}
}

func TestStopOnlyRetriesAuthoritativeInterruptedExecution(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-interrupted-stop", "interrupted stop")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID:    "pilot-interrupted-stop",
		RobotID:            "robot-interrupted-stop",
		RobotStatus:        "interrupted",
		CurrentExecutionID: "rex-current-interrupted",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	now := time.Now().UTC()
	for _, execution := range []store.RobotExecution{
		{
			ID: "rex-current-interrupted", ProjectID: project.ID,
			RobotID: "robot-interrupted-stop", PilotInstanceID: "pilot-interrupted-stop",
			SkillName: "grasp-object", SkillVersion: "0.2.0", RequestKey: "current",
			Status: "interrupted", Revision: 2, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "rex-history-interrupted", ProjectID: project.ID,
			RobotID: "robot-interrupted-stop", PilotInstanceID: "pilot-interrupted-stop",
			SkillName: "grasp-object", SkillVersion: "0.2.0", RequestKey: "history",
			Status: "interrupted", Revision: 2, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
		},
	} {
		if err := st.SaveRobotExecution(execution); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := service.Stop(context.Background(), project.ID,
		"rex-history-interrupted", "operator"); !errors.Is(err, ErrExecutionNotActive) {
		t.Fatalf("历史 interrupted 不得伪装为可停止执行，err=%v", err)
	}
	select {
	case command := <-commands:
		t.Fatalf("历史执行不应向 Pilot 下发 stop: %#v", command)
	default:
	}

	stopping, err := service.Stop(context.Background(), project.ID,
		"rex-current-interrupted", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if stopping.Status != "stopping" {
		t.Fatalf("当前 interrupted 应允许重试安全停止: %#v", stopping)
	}
	command := receiveCommand(t, commands, "execution.stop")
	payload, _ := command.Payload.(map[string]any)
	if payload["execution_id"] != "rex-current-interrupted" {
		t.Fatalf("stop 下发到了错误执行: %#v", command.Payload)
	}
}

func TestRobotServiceRegistryRunArtifactAndStopVerticalFlow(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-robot", "Robot Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{Name: "grasp-object", Version: "0.1.0", Category: "robot_skill",
		RequiredActions: []map[string]any{{"type": "gripper.close", "schema_version": 1}}, StopActions: []map[string]any{{"type": "gripper.hold_object", "schema_version": 1}}, PublishedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	events := &recordingRobotEvents{}
	service := NewService(st, events)
	pilot := store.RobotPilot{PilotInstanceID: "pilot-1", RobotID: "r1pro-1", DisplayName: "R1 Pro 1", RobotModel: "r1pro",
		Backend: "fake", PilotVersion: "0.5.0", RobotStatus: "idle", AbilityFrameworkStatus: "ready"}
	commands, disconnect, err := service.Connect(pilot)
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := service.HandlePilotEvent("pilot-1", "skill.status", 0, map[string]any{"name": "grasp-object", "version": "0.1.0", "status": "installed", "enabled": true}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := service.DeviceSnapshot()
	if err != nil || len(snapshot.Robots) != 1 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	deviceEvents, unsubscribe, err := service.SubscribeDevices(snapshot.EventSequence)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()

	workflowID := prepareVerticalFlowTask(t, st, project)
	request := RunRequest{ProjectID: project.ID, WorkflowID: workflowID, TaskID: "task-1", SubtaskID: "subtask-1", RunID: "run-1",
		RobotID: "r1pro-1", SkillName: "grasp-object", SkillVersion: "0.1.0", RequestKey: "request-1", Input: map[string]any{"target_ref": "box-1"}}
	execution, err := runWithValidSkillInput(t, service, commands, "pilot-1", request)
	if err != nil {
		t.Fatal(err)
	}
	start := receiveCommand(t, commands, "execution.start")
	payload := start.Payload.(map[string]any)
	downlinked := payload["execution"].(store.RobotExecution)
	if downlinked.ID != execution.ID || downlinked.TaskID != "task-1" {
		t.Fatalf("downlink=%#v", downlinked)
	}
	duplicate, err := service.Run(context.Background(), request)
	if err != nil || duplicate.ID != execution.ID {
		t.Fatalf("幂等 run=%#v err=%v", duplicate, err)
	}
	select {
	case duplicateCommand := <-commands:
		t.Fatalf("幂等 run 重复下发: %#v", duplicateCommand)
	default:
	}

	if err := service.HandlePilotEvent("pilot-1", "skill.started", 1, map[string]any{
		"execution_id": execution.ID, "skill_status": "running", "status": "running",
	}); err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"execution_id": execution.ID, "status": "running", "skill_status": "running", "stage": "grasp", "stage_status": "running",
		"expectation": "双侧接触", "observation_summary": "左侧接触", "deviation": "右侧未接触", "next_step": "continue", "evidence_refs": []any{}}
	if err := service.HandlePilotEvent("pilot-1", "event.report", 2, report); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetRobotExecution(execution.ID)
	if err != nil || updated.Stage != "grasp" || updated.Status != "running" {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	// Action 的 succeeded 只表示一次 Ability Task 已终结；Robot Skill 可能还要
	// 独立验证本 Stage，不能因此把顶层 Execution 标为完成或释放 Robot 锁。
	if err := service.HandlePilotEvent("pilot-1", "action.terminal", 3, map[string]any{
		"execution_id": execution.ID, "skill_status": "running", "status": "succeeded",
		"stage": "grasp", "action_id": "action-1",
	}); err != nil {
		t.Fatal(err)
	}
	updated, err = st.GetRobotExecution(execution.ID)
	if err != nil || updated.Status != "running" {
		t.Fatalf("Action 终态覆盖了 Robot Execution: updated=%#v err=%v", updated, err)
	}
	if err := service.HandlePilotEvent("pilot-1", "agent.requested", 4, map[string]any{
		"execution_id": execution.ID, "stage": "grasp",
		"decision_key": "candidate-exhausted", "decision_revision": float64(2),
		"reason": "候选已耗尽", "response_model": "GraspRecoveryDecisionV1",
	}); err != nil {
		t.Fatal(err)
	}
	updated, err = st.GetRobotExecution(execution.ID)
	if err != nil || updated.Status != "waiting_agent" {
		t.Fatalf("agent.requested 没有持久化 waiting_agent: updated=%#v err=%v", updated, err)
	}
	if err := service.HandlePilotEvent("pilot-1", "agent.resolved", 5, map[string]any{
		"execution_id": execution.ID, "stage": "grasp",
		"decision_key": "candidate-exhausted", "status": "running",
	}); err != nil {
		t.Fatal(err)
	}
	updated, _ = st.GetRobotExecution(execution.ID)
	if updated.Status != "running" {
		t.Fatalf("agent.resolved 没有恢复 Skill running: %+v", updated)
	}
	lastProject := events.items[len(events.items)-1]
	projectPayload, ok := lastProject.Payload.(map[string]any)
	if !ok || projectPayload["execution"] == nil || projectPayload["event"] == nil {
		t.Fatalf("Project WS payload=%#v", lastProject.Payload)
	}

	if err := service.HandlePilotEvent("pilot-1", "artifact.announce", 0, map[string]any{"execution_id": execution.ID,
		"local_artifact_id": "rgb-1", "media_type": "image/png", "summary": "抓取证据", "size_bytes": 7}); err != nil {
		t.Fatal(err)
	}
	upload := receiveCommand(t, commands, "artifact.upload")
	uploadPayload := upload.Payload.(map[string]any)
	uploadURL := uploadPayload["upload_url"].(string)
	recorder := httptest.NewRecorder()
	httpRequest := httptest.NewRequest(http.MethodPut, uploadURL, bytes.NewReader([]byte("PNGDATA")))
	service.TransferHandler(recorder, httpRequest, "pilot-1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("artifact upload status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var uploadResult map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &uploadResult); err != nil {
		t.Fatal(err)
	}
	if uploadResult["sync_status"] != "synced" {
		t.Fatalf("upload result=%#v", uploadResult)
	}
	updated, _ = st.GetRobotExecution(execution.ID)
	if len(updated.ArtifactRefs) != 1 || len(updated.ArtifactSync) != 1 || updated.ArtifactSync[0].Status != "synced" {
		t.Fatalf("execution artifact state=%#v", updated)
	}
	// Pilot 的 Skill 终态可能基于上传前读取的 Execution 保存旧 body。
	// 重新读取必须仍以独立映射表里的 synced 为准。
	stale := updated
	stale.ArtifactSync = nil
	if err := st.SaveRobotExecution(stale); err != nil {
		t.Fatal(err)
	}
	updated, _ = st.GetRobotExecution(execution.ID)
	if len(updated.ArtifactSync) != 1 || updated.ArtifactSync[0].Status != "synced" {
		t.Fatalf("stale execution overwrote artifact mapping: %#v", updated)

	}
	stopping, err := service.Stop(context.Background(), project.ID, execution.ID, "operator stop")
	if err != nil || stopping.Status != "stopping" {
		t.Fatalf("stop=%#v err=%v", stopping, err)
	}
	receiveCommand(t, commands, "execution.stop")
	if err := service.HandlePilotEvent("pilot-1", "agent.requested", 100, map[string]any{
		"execution_id": execution.ID, "stage": "grasp",
		"decision_key": "late-request", "decision_revision": float64(3),
	}); err != nil {
		t.Fatal(err)
	}
	stillStopping, err := st.GetRobotExecution(execution.ID)
	if err != nil || stillStopping.Status != "stopping" {
		t.Fatalf("迟到 waiting_agent 不得覆盖 stopping: execution=%#v err=%v",
			stillStopping, err)
	}

	seenExecution := false
	seenArtifact := false
	deadline := time.After(time.Second)
	for !(seenExecution && seenArtifact) {
		select {
		case item := <-deviceEvents:
			seenExecution = seenExecution || item.ResourceType == "robot_execution"
			seenArtifact = seenArtifact || item.ResourceType == "artifact_sync"
		case <-deadline:
			t.Fatalf("设备事件缺失 execution=%v artifact=%v", seenExecution, seenArtifact)
		}
	}
}

func TestRobotServicePublishesAbilityDebugStatusToDeviceStream(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	_, disconnect, err := service.Connect(store.RobotPilot{PilotInstanceID: "pilot-debug",
		RobotID: "r1pro-debug", RobotStatus: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	snapshot, err := service.DeviceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe, err := service.SubscribeDevices(snapshot.EventSequence)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	debug := map[string]any{"id": "debug-1", "robot_id": "r1pro-debug", "status": "running", "revision": float64(2)}
	if err := service.HandlePilotEvent("pilot-debug", "ability.debug.status", 0, map[string]any{"debug": debug}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		payload, _ := event.Payload.(map[string]any)
		if event.ResourceType != "ability_debug" || event.Type != "ability_debug.running" || payload["debug"] == nil {
			t.Fatalf("unexpected device event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("Ability debug event was not published")
	}
}

func TestRobotServiceCompletedExecutionConvergesProgress(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-progress", "progress")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	_, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-progress", RobotID: "robot-progress", RobotStatus: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	now := time.Now().UTC()
	partial := 0.27
	execution := store.RobotExecution{
		ID: "rex-progress", ProjectID: project.ID, RobotID: "robot-progress",
		PilotInstanceID: "pilot-progress", SkillName: "grasp-object",
		SkillVersion: "0.2.0", RequestKey: "progress", Status: "running",
		Progress: &partial, Error: map[string]any{"code": "RECOVERED_ACTION"},
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := service.HandlePilotEvent("pilot-progress", "execution.terminal", 1, map[string]any{
		"execution_id": execution.ID, "skill_status": "completed",
		"result": map[string]any{"done": true},
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := st.GetRobotExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Progress == nil || *completed.Progress != 1 {
		t.Fatalf("completed Execution 必须收敛到 100%%: %#v", completed.Progress)
	}
	if completed.Error != nil {
		t.Fatalf("completed Execution 不应保留已恢复 Action 的顶层错误: %#v", completed.Error)
	}
}

func TestRobotServiceStopAndWaitUsesPersistedHoldEvidence(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-hold", "hold")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-hold", RobotID: "robot-hold", RobotStatus: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-hold", ProjectID: project.ID, RobotID: "robot-hold",
		PilotInstanceID: "pilot-hold", SkillName: "semantic-navigation",
		SkillVersion: "0.2.0", RequestKey: "hold", Status: "running",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	pilot, _ := st.GetRobotPilot("pilot-hold")
	service.setPilotExecution(pilot, execution.ID)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, waitErr := service.StopAndWait(ctx, project.ID, execution.ID, "scene_reset")
		result <- waitErr
	}()
	receiveCommand(t, commands, "execution.stop")
	if err := service.HandlePilotEvent("pilot-hold", "skill.stop.finalized", 1, map[string]any{
		"execution_id": execution.ID, "skill_status": "stopped",
		"stop_outcome": map[string]any{"safe": true, "hold_confirmed": true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("没有复用 Pilot 安全停止证据: %v", err)
	}
	stopped, _ := st.GetRobotExecution(execution.ID)
	if stopped.Result["safe"] != true || stopped.Result["hold_confirmed"] != true {
		t.Fatalf("stop outcome 没有进入 Execution result: %#v", stopped.Result)
	}
}

func TestAbilityDebugOnlyStartsManifestDeclaredTask(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-debug-task", RobotID: "r1pro-debug-task", RobotStatus: "idle",
		Abilities: []map[string]any{{
			"instance_id": "ability-navigation", "tasks": []string{"PlanRoute", "GetExecution", "StopExecution"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if _, err := service.StartAbilityDebug("r1pro-debug-task", "ability-navigation", "UnknownTask", nil); !errors.Is(err, ErrAbilityTaskUnknown) {
		t.Fatalf("未声明 Task 不得发送到 Pilot: %v", err)
	}
	select {
	case command := <-commands:
		t.Fatalf("未声明 Task 仍产生了命令: %#v", command)
	default:
	}
	if _, err := service.StartAbilityDebug("r1pro-debug-task", "ability-navigation", "GetExecution", nil); !errors.Is(err, ErrAbilityTaskUnknown) {
		t.Fatalf("管理 Task 不得作为调试 Task: %v", err)
	}
	debug, err := service.StartAbilityDebug("r1pro-debug-task", "ability-navigation", "PlanRoute", map[string]any{"target": "gate-b"})
	if err != nil || debug["ability_instance_id"] != "ability-navigation" {
		t.Fatalf("Manifest Task 应精确发送到指定实例: debug=%#v err=%v", debug, err)
	}
	command := receiveCommand(t, commands, "ability.debug.start")
	payload, ok := command.Payload.(map[string]any)
	if !ok || payload["task_name"] != "PlanRoute" || payload["ability_instance_id"] != "ability-navigation" {
		t.Fatalf("Ability debug command=%#v", command)
	}
}

func TestRobotServicePersistsFailedPilotCommandAck(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-install", RobotID: "robot-install", RobotStatus: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: "semantic-navigation", Version: "0.1.0", PackagePath: filepath.Join(t.TempDir(), "package.zip"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.InstallSkill("pilot-install", "semantic-navigation", "0.1.0"); err != nil {
		t.Fatal(err)
	}
	command := receiveCommand(t, commands, "skill.install")
	ackErr := service.HandleCommandAck("pilot-install", command.CommandID, false, "HTTP 401")
	if ackErr == nil || !strings.Contains(ackErr.Error(), "HTTP 401") {
		t.Fatalf("失败 ack 没有向调用链暴露: %v", ackErr)
	}
	item, err := st.GetRobotPilotSkill("pilot-install", "semantic-navigation", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "failed" || item.Error != "HTTP 401" {
		t.Fatalf("失败 ack 未持久化: %#v", item)
	}
	if err := service.HandleCommandAck("pilot-install", command.CommandID, false, "duplicate"); err == nil {
		t.Fatal("重复或未知 ack 必须拒绝")
	}
}

func TestRobotServiceConflictingRequestKeyIsRejected(t *testing.T) {
	st := openRobotTestStore(t)
	project, _ := st.CreateProject("user", "Project")
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{PilotInstanceID: "pilot", RobotID: "robot", RobotStatus: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	_ = st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "pilot", Name: "skill", Version: "1", Enabled: true, Status: "installed"})
	first := RunRequest{ProjectID: project.ID, RobotID: "robot", SkillName: "skill", SkillVersion: "1", RequestKey: "same", Input: map[string]any{"target": "a"}}
	if _, err := runWithValidSkillInput(t, service, commands, "pilot", first); err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.start")
	first.Input["target"] = "b"
	if _, err := service.Run(context.Background(), first); err != ErrRequestConflict {
		t.Fatalf("err=%v", err)
	}
}

func TestRuntimeAppearsBeforePilotAndMergesAfterReady(t *testing.T) {
	st := openRobotTestStore(t)
	events := &recordingRobotEvents{}
	service := NewService(st, events)
	if _, err := service.Device("missing-robot"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Pilot/Runtime 都不存在时应返回 Robot not-found: %v", err)
	}
	deviceEvents, unsubscribe, err := service.SubscribeDevices(0)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()

	now := time.Now().UTC()
	instance := robotruntime.RuntimeInstance{InstanceID: "runtime-pre-pilot", PilotInstanceID: "pilot-pre",
		RobotID: "robot-pre", RobotModel: "R1Pro", Backend: "fake", Kind: "simulation",
		BackendProfile: "r1pro-fake-v1", Status: robotruntime.StateStarting,
		ProjectID: "project-runtime",
		Revision:  1, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRuntimeInstance(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if err := service.PublishRuntimeEvent(context.Background(), robotruntime.Event{Type: "robot.runtime.starting",
		RobotID: instance.RobotID, ProjectID: instance.ProjectID, Instance: instance}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.DeviceSnapshot()
	if err != nil || len(snapshot.Robots) != 1 {
		t.Fatalf("Pilot 前设备快照错误: %#v err=%v", snapshot, err)
	}
	view := snapshot.Robots[0]
	if view["robot_id"] != instance.RobotID || view["backend"] != "fake" || view["environment"] != "simulation" {
		t.Fatalf("Runtime synthetic device 字段错误: %#v", view)
	}
	select {
	case event := <-deviceEvents:
		payload := event.Payload.(map[string]any)
		if event.ResourceType != "robot_runtime_instance" || event.ResourceID != instance.InstanceID ||
			payload["robot_id"] != instance.RobotID {
			t.Fatalf("Runtime 设备事件结构错误: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到 Runtime 设备事件")
	}
	if len(events.items) != 1 || events.items[0].ResourceType != "robot_runtime_instance" ||
		events.items[0].ProjectID != instance.ProjectID {
		t.Fatalf("Runtime Project 事件错误: %#v", events.items)
	}

	pilot := store.RobotPilot{PilotInstanceID: instance.PilotInstanceID, RobotID: instance.RobotID,
		RobotModel: instance.RobotModel, Backend: instance.Backend}
	commands, disconnect, err := service.Connect(pilot)
	if err != nil {
		t.Fatalf("受管 Runtime 必须允许精确 Pilot 在 starting 阶段注册: %v", err)
	}
	_ = commands
	defer disconnect()
	instance.Status = robotruntime.StateReady
	instance.Revision++
	instance.UpdatedAt = now.Add(time.Second)
	if err := st.SaveRuntimeInstance(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	merged, err := service.DeviceSnapshot()
	if err != nil || len(merged.Robots) != 1 {
		t.Fatalf("Runtime ready 后没有按 robot_id 合并: %#v err=%v", merged, err)
	}
	if merged.Robots[0]["runtime_instance"] == nil {
		t.Fatalf("合并 Pilot 后丢失 Runtime: %#v", merged.Robots[0])
	}
}

func TestPilotEnrollmentCredentialIsSingleUseAndIdentityBound(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	enrollment, err := service.CreatePilotEnrollment("user-1")
	if err != nil || len(enrollment.Code) != 6 {
		t.Fatalf("CreatePilotEnrollment=%+v err=%v", enrollment, err)
	}
	_, credential, err := service.ClaimPilotEnrollment(enrollment.Code, "pilot-r1pro-1")
	if err != nil || credential == "" {
		t.Fatalf("ClaimPilotEnrollment credential=%q err=%v", credential, err)
	}
	pilotID, err := service.ValidatePilotCredential(credential)
	if err != nil || pilotID != "pilot-r1pro-1" {
		t.Fatalf("ValidatePilotCredential pilot=%q err=%v", pilotID, err)
	}
	if _, _, err := service.ClaimPilotEnrollment(enrollment.Code, "pilot-other"); !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("加入码必须只能使用一次: %v", err)
	}
}

func TestDesiredSkillsInstallAndConvergeToPilotActual(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{Name: "grasp-object", Version: "0.1.0",
		PackagePath: filepath.Join(t.TempDir(), "grasp-object.zip")}); err != nil {
		t.Fatal(err)
	}
	commands, disconnect, err := service.Connect(store.RobotPilot{PilotInstanceID: "pilot-1",
		RobotID: "r1pro-1", RobotModel: "r1pro", Backend: "fake", DesiredSkills: []store.RobotDesiredSkill{{
			Name: "grasp-object", Version: "0.1.0", Enabled: true,
		}}})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	command := receiveCommand(t, commands, "skill.install")
	payload := command.Payload.(map[string]any)
	if payload["name"] != "grasp-object" || payload["version"] != "0.1.0" {
		t.Fatalf("unexpected install payload: %#v", payload)
	}
	if err := service.HandlePilotEvent("pilot-1", "skill.status", 0, map[string]any{
		"name": "grasp-object", "version": "0.1.0", "enabled": true, "status": "installed",
	}); err != nil {
		t.Fatal(err)
	}
	desired, err := st.ListRobotDesiredSkills("r1pro-1")
	if err != nil || len(desired) != 1 || !desired[0].Enabled {
		t.Fatalf("desired=%+v err=%v", desired, err)
	}
	device, err := service.Device("r1pro-1")
	if err != nil || device["desired_skills"] == nil || device["installed_skills"] == nil {
		t.Fatalf("device desired/actual not exposed: %#v err=%v", device, err)
	}
}

func TestDesiredSkillReconcileDoesNotRepeatInflightCommands(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: "grasp-object", Version: "0.2.0",
		PackagePath: filepath.Join(t.TempDir(), "grasp-object.zip"),
	}); err != nil {
		t.Fatal(err)
	}
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-inflight", RobotID: "robot-inflight",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if _, err := service.SetDesiredSkill("robot-inflight", "grasp-object", "0.2.0", true); err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileDesiredSkills("robot-inflight"); err != nil {
		t.Fatal(err)
	}
	install := receiveCommand(t, commands, "skill.install")
	select {
	case duplicate := <-commands:
		t.Fatalf("同一包安装在途时不得重复下发: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
	// Pilot 的协议顺序是先报告 actual、再回复当前 command.ack。actual 事件
	// 触发的对账必须等待安装 ack，不能在这个窗口重复 install 或抢先 enable。
	if err := service.HandlePilotEvent("pilot-inflight", "skill.status", 0, map[string]any{
		"name": "grasp-object", "version": "0.2.0", "enabled": false, "status": "installed",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-commands:
		t.Fatalf("安装 ack 前不得下发后续命令: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
	if err := service.HandleCommandAck("pilot-inflight", install.CommandID, true, ""); err != nil {
		t.Fatal(err)
	}
	enable := receiveCommand(t, commands, "skill.enable")
	if err := service.HandlePilotEvent("pilot-inflight", "skill.status", 0, map[string]any{
		"name": "grasp-object", "version": "0.2.0", "enabled": true, "status": "installed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleCommandAck("pilot-inflight", enable.CommandID, true, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-commands:
		t.Fatalf("desired/actual 收敛后不得继续下发: %#v", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDesiredSkillReconnectDropsCommandsFromOldSession(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: "grasp-object", Version: "0.2.0",
		PackagePath: filepath.Join(t.TempDir(), "grasp-object.zip"),
	}); err != nil {
		t.Fatal(err)
	}
	firstCommands, firstDisconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-reconnect", RobotID: "robot-reconnect",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetDesiredSkill("robot-reconnect", "grasp-object", "0.2.0", true); err != nil {
		t.Fatal(err)
	}
	_ = receiveCommand(t, firstCommands, "skill.install")
	firstDisconnect()

	secondCommands, secondDisconnect, err := service.ConnectWithSkillSnapshot(store.RobotPilot{
		PilotInstanceID: "pilot-reconnect", RobotID: "robot-reconnect",
	}, []store.RobotPilotSkill{})
	if err != nil {
		t.Fatal(err)
	}
	defer secondDisconnect()
	command := receiveCommand(t, secondCommands, "skill.install")
	if command.Type != "skill.install" {
		t.Fatalf("重连后没有从新快照重新对账: %#v", command)
	}
}

func TestDesiredSkillCanBeInstalledAgainAfterPilotReportsUninstalled(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: "grasp-object", Version: "0.2.0",
		PackagePath: filepath.Join(t.TempDir(), "grasp-object.zip"),
	}); err != nil {
		t.Fatal(err)
	}
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-reinstall", RobotID: "r1pro-reinstall",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: "pilot-reinstall", Name: "grasp-object", Version: "0.2.0",
		Enabled: false, Status: "uninstalled", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetDesiredSkill("r1pro-reinstall", "grasp-object", "0.2.0", true); err != nil {
		t.Fatal(err)
	}
	command := receiveCommand(t, commands, "skill.install")
	payload := command.Payload.(map[string]any)
	if payload["name"] != "grasp-object" || payload["version"] != "0.2.0" {
		t.Fatalf("unexpected reinstall payload: %#v", payload)
	}
}

type recordingAvailabilityObserver struct {
	robots chan string
}

func (o *recordingAvailabilityObserver) OnRobotAvailabilityChanged(_ context.Context, robotID string) {
	o.robots <- robotID
}

func TestConnectPublishesRobotAvailabilityEdge(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	observer := &recordingAvailabilityObserver{robots: make(chan string, 1)}
	service.SetAvailabilityObserver(observer)
	_, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-available", RobotID: "r1pro-available",
		Status: "online", RobotStatus: "idle", AbilityFrameworkStatus: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	select {
	case robotID := <-observer.robots:
		if robotID != "r1pro-available" {
			t.Fatalf("可用事件 Robot 错误: %s", robotID)
		}
	case <-time.After(time.Second):
		t.Fatal("Pilot ready 后没有发布 Robot 可用事件")
	}
}

func TestConfirmManagedSimulationHoldOnlyConvergesCurrentExecution(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-managed-hold", "managed hold")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SaveRobotPilot(store.RobotPilot{
		PilotInstanceID: "pilot-managed", RobotID: "robot-managed",
		Status: "offline", RobotStatus: "interrupted",
		CurrentExecutionID: "rex-current", Revision: 1, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	current := store.RobotExecution{
		ID: "rex-current", ProjectID: project.ID, RobotID: "robot-managed",
		PilotInstanceID: "pilot-managed", SkillName: "grasp-object",
		SkillVersion: "0.2.0", RequestKey: "current", Status: "interrupted",
		Error: map[string]any{"code": "PILOT_OFFLINE"}, Revision: 2,
		CreatedAt: now, UpdatedAt: now,
	}
	history := current
	history.ID = "rex-history"
	history.RequestKey = "history"
	for _, execution := range []store.RobotExecution{current, history} {
		if err := st.SaveRobotExecution(execution); err != nil {
			t.Fatal(err)
		}
	}

	service := NewService(st, nil)
	if err := service.ConfirmManagedSimulationHold(
		context.Background(), project.ID, "robot-managed", current.ID, "scene_stop",
	); err != nil {
		t.Fatal(err)
	}
	stopped, err := st.GetRobotExecution(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != "stopped" || stopped.Result["safe"] != true ||
		stopped.Result["hold_confirmed"] != true || stopped.Error != nil {
		t.Fatalf("当前 Execution 没有按 Runtime hold 证据收敛: %#v", stopped)
	}
	unchanged, err := st.GetRobotExecution(history.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != "interrupted" {
		t.Fatalf("历史 Execution 被越界改写: %#v", unchanged)
	}
	pilot, err := st.GetRobotPilot("pilot-managed")
	if err != nil {
		t.Fatal(err)
	}
	if pilot.CurrentExecutionID != "" {
		t.Fatalf("当前 Execution 锁没有释放: %#v", pilot)
	}
}

func TestDeviceSnapshotDoesNotPresentPersistedPilotAsOnline(t *testing.T) {
	st := openRobotTestStore(t)
	now := time.Now().UTC()
	if err := st.SaveRobotPilot(store.RobotPilot{
		PilotInstanceID: "pilot-stale", RobotID: "robot-stale",
		Status: "online", RobotStatus: "busy", AbilityFrameworkStatus: "ready",
		Abilities:          []map[string]any{{"instance_id": "ability-stale", "healthy": true}},
		CurrentExecutionID: "rex-stale", Revision: 3, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	snapshot, err := service.DeviceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	robots, _ := document["robots"].([]any)
	if len(robots) != 1 {
		t.Fatalf("robots=%#v", document["robots"])
	}
	device := robots[0].(map[string]any)
	pilot := device["pilot"].(map[string]any)
	framework := device["ability_framework"].(map[string]any)
	abilities := device["abilities"].([]any)
	if device["status"] != "offline" || pilot["status"] != "offline" ||
		framework["status"] != "offline" || framework["healthy_instances"] != float64(0) {
		t.Fatalf("断线 Pilot 的缓存状态仍被展示为在线: %#v", device)
	}
	if len(abilities) != 1 || abilities[0].(map[string]any)["status"] != "offline" ||
		abilities[0].(map[string]any)["health"] != "unknown" ||
		abilities[0].(map[string]any)["healthy"] != false {
		t.Fatalf("断线 Pilot 的 Ability 历史目录仍被展示为运行中: %#v", abilities)
	}
}

func TestReconcileReturnsOnlyRecoverableExecutions(t *testing.T) {
	st := openRobotTestStore(t)
	now := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	pilot := store.RobotPilot{PilotInstanceID: "pilot-reconcile", RobotID: "robot-reconcile",
		Status: "online", RobotStatus: "busy", LastSeenAt: now, Revision: 1}
	if err := st.SaveRobotPilot(pilot); err != nil {
		t.Fatal(err)
	}
	completed := store.RobotExecution{
		ID: "rex-completed", ProjectID: "project-reconcile", RobotID: pilot.RobotID,
		PilotInstanceID: pilot.PilotInstanceID, SkillName: "grasp-object", SkillVersion: "0.2.0",
		RequestKey: "completed", Status: "completed", Input: map[string]any{},
		Result:   map[string]any{"large_history": strings.Repeat("x", 64<<10)},
		Revision: 2, CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	running := store.RobotExecution{
		ID: "rex-running", ProjectID: "project-reconcile", RobotID: pilot.RobotID,
		PilotInstanceID: pilot.PilotInstanceID, SkillName: "semantic-navigation", SkillVersion: "0.2.0",
		RequestKey: "running", Status: "running", Input: map[string]any{},
		Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	for _, execution := range []store.RobotExecution{completed, running} {
		if err := st.SaveRobotExecution(execution); err != nil {
			t.Fatal(err)
		}
	}

	service := NewService(st, nil)
	service.now = func() time.Time { return now }
	snapshot, err := service.Reconcile(pilot.PilotInstanceID, []map[string]any{{
		"execution_id": running.ID, "status": "running",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].ID != running.ID {
		t.Fatalf("reconcile snapshot contains terminal history: %#v", snapshot)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) >= 32<<10 {
		t.Fatalf("recoverable snapshot unexpectedly includes large terminal payload: %d bytes", len(body))
	}
}
