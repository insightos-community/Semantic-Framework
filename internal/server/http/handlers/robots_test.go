package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

func newRobotHandlerTest(t *testing.T) (http.Handler, *store.Store, *robotdomain.Service, <-chan any) {
	t.Helper()
	st := storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	service := robotdomain.NewService(st, nil)
	handler := NewRobotsHandler(service, st)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUserID(r.Context(), r.Header.Get("X-Test-User"))))
		})
	})
	router.Get("/api/v1/devices/snapshot", handler.HandleDeviceSnapshot)
	router.Get("/api/v1/devices", handler.HandleListDevices)
	router.Get("/api/v1/devices/{robot_id}", handler.HandleGetDevice)
	router.Get("/api/v1/robot-executions/{execution_id}", handler.HandleGetExecution)
	router.Get("/api/v1/projects/{id}/robot-executions", handler.HandleListProjectExecutions)
	router.Post("/api/v1/projects/{id}/robots/{robot_id}/skill-executions", handler.HandleStartSkillDebug)
	router.Post("/api/v1/devices/{robot_id}/stop", handler.HandleStopDevice)
	router.Route("/api/v1/robot-skills", func(r chi.Router) {
		r.Get("/", handler.HandleListRobotSkills)
		r.Post("/", handler.HandlePublishRobotSkill)
		r.Get("/{name}/{version}", handler.HandleGetRobotSkill)
		r.Get("/{name}/{version}/resources/*", handler.HandleGetRobotSkillResource)
	})
	return router, st, service, nil
}

func requestRobotJSON(t *testing.T, router http.Handler, method, path, user string, body []byte) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("X-Test-User", user)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	var result map[string]any
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	}
	return recorder.Code, result
}

func TestDeviceHTTPContractMatchesStudio(t *testing.T) {
	router, st, service, _ := newRobotHandlerTest(t)
	project, err := st.CreateProject("user-devices", "Devices")
	if err != nil {
		t.Fatal(err)
	}
	_, disconnect, err := service.Connect(store.RobotPilot{PilotInstanceID: "pilot-1", RobotID: "robot-1", DisplayName: "Robot 1",
		RobotModel: "r1pro", Backend: "fake", PilotVersion: "0.5.0", RobotStatus: "busy", AbilityFrameworkStatus: "ready",
		Abilities: []map[string]any{{"instance_id": "navigation-1", "health": "healthy"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "pilot-1", Name: "grasp-object", Version: "0.1.0", Status: "installed", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{Name: "grasp-object", Version: "0.1.0", Category: "robot_skill", PublishedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	execution := store.RobotExecution{ID: "rex-1", ProjectID: project.ID, RobotID: "robot-1", PilotInstanceID: "pilot-1",
		SkillName: "grasp-object", SkillVersion: "0.1.0", RequestKey: "request-1", Status: "running", Stage: "grasp", Revision: 3,
		Input: map[string]any{}, ArtifactRefs: []string{}, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	code, executionDetail := requestRobotJSON(t, router, http.MethodGet, "/api/v1/robot-executions/rex-1", "user-devices", nil)
	events, eventsOK := executionDetail["events"].([]any)
	if code != http.StatusOK || !eventsOK || len(events) != 0 {
		t.Fatalf("新 Robot Execution 的 events 必须为空数组：code=%d body=%#v", code, executionDetail)
	}

	code, snapshotBody := requestRobotJSON(t, router, http.MethodGet, "/api/v1/devices/snapshot", "user-devices", nil)
	if code != http.StatusOK || snapshotBody["snapshot"] == nil {
		t.Fatalf("snapshot code=%d body=%#v", code, snapshotBody)
	}
	snapshot := snapshotBody["snapshot"].(map[string]any)
	robot := snapshot["robots"].([]any)[0].(map[string]any)
	if robot["robot_id"] != "robot-1" || robot["installed_skills"] == nil || robot["abilities"] == nil {
		t.Fatalf("robot=%#v", robot)
	}
	code, listBody := requestRobotJSON(t, router, http.MethodGet, "/api/v1/devices?status=busy", "user-devices", nil)
	listed := listBody["devices"].([]any)
	if code != http.StatusOK || len(listed) != 1 || listed[0].(map[string]any)["robot_id"] != "robot-1" {
		t.Fatalf("设备列表必须复用快照中的实时状态：code=%d body=%#v", code, listBody)
	}

	code, detail := requestRobotJSON(t, router, http.MethodGet, "/api/v1/devices/robot-1", "user-devices", nil)
	if code != http.StatusOK || detail["device"] == nil || detail["executions"] == nil || detail["skill_packages"] == nil {
		t.Fatalf("detail code=%d body=%#v", code, detail)
	}
	gotExecution := detail["executions"].([]any)[0].(map[string]any)
	if gotExecution["id"] != "rex-1" {
		t.Fatalf("REST execution 必须使用 id: %#v", gotExecution)
	}

	code, projectExecutions := requestRobotJSON(t, router, http.MethodGet, "/api/v1/projects/"+project.ID+"/robot-executions", "user-devices", nil)
	if code != http.StatusOK || len(projectExecutions["executions"].([]any)) != 1 {
		t.Fatalf("project executions=%#v", projectExecutions)
	}
	code, _ = requestRobotJSON(t, router, http.MethodGet, "/api/v1/projects/"+project.ID+"/robot-executions", "other-user", nil)
	if code != http.StatusNotFound {
		t.Fatalf("跨用户 Project 应隐藏，code=%d", code)
	}
}

func TestRobotExecutionDetailPaginatesLongTimeline(t *testing.T) {
	router, st, _, _ := newRobotHandlerTest(t)
	project, err := st.CreateProject("user-pages", "Execution pages")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	execution := store.RobotExecution{ID: "rex-pages", ProjectID: project.ID,
		RobotID: "robot-pages", PilotInstanceID: "pilot-pages", SkillName: "semantic-navigation",
		SkillVersion: "0.4.0", RequestKey: "request-pages", Status: "completed", Revision: 1,
		Input: map[string]any{}, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	for sequence := int64(1); sequence <= 502; sequence++ {
		if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
			ExecutionID: execution.ID, Sequence: sequence, Type: "stage.progress",
			Payload: map[string]any{"stage": "navigate"}, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	code, first := requestRobotJSON(t, router, http.MethodGet,
		"/api/v1/robot-executions/rex-pages", "user-pages", nil)
	if code != http.StatusOK || len(first["events"].([]any)) != 500 ||
		first["has_more"] != true || first["next_sequence"] != float64(500) {
		t.Fatalf("长时间线第一页分页错误: code=%d body=%#v", code, first)
	}
	code, second := requestRobotJSON(t, router, http.MethodGet,
		"/api/v1/robot-executions/rex-pages?after_sequence=500", "user-pages", nil)
	if code != http.StatusOK || len(second["events"].([]any)) != 2 ||
		second["has_more"] != false || second["next_sequence"] != float64(502) {
		t.Fatalf("长时间线末页分页错误: code=%d body=%#v", code, second)
	}
}

func TestStartSkillDebugCreatesStandaloneRobotExecution(t *testing.T) {
	router, st, service, _ := newRobotHandlerTest(t)
	project, err := st.CreateProject("user-debug", "Skill Debug")
	if err != nil {
		t.Fatal(err)
	}
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-debug", RobotID: "robot-debug", DisplayName: "Robot Debug",
		RobotModel: "r1pro", Backend: "fake", RobotStatus: "idle", AbilityFrameworkStatus: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: "pilot-debug", Name: "grasp-object", Version: "0.2.0",
		Status: "installed", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{
		"skill_name":"grasp-object",
		"skill_version":"0.2.0",
		"request_key":"manual-debug-1",
		"input":{"target":{"object_ref":"tote-1"}}
	}`)
	path := "/api/v1/projects/" + project.ID + "/robots/robot-debug/skill-executions"
	type requestResult struct {
		code int
		body map[string]any
	}
	requestDone := make(chan requestResult, 1)
	go func() {
		code, response := requestRobotJSON(t, router, http.MethodPost, path, "user-debug", body)
		requestDone <- requestResult{code: code, body: response}
	}()
	command := <-commands
	if command.Type != "skill.validate_input" {
		t.Fatalf("人工 Skill 调试应先校验输入，实际命令=%s", command.Type)
	}
	if err := service.HandleCommandAckResult("pilot-debug", command.CommandID, true, "",
		map[string]any{"valid": true, "input": map[string]any{"target": map[string]any{"object_ref": "tote-1"}}}); err != nil {
		t.Fatal(err)
	}
	requestOutcome := <-requestDone
	code, response := requestOutcome.code, requestOutcome.body
	if code != http.StatusAccepted {
		t.Fatalf("人工 Skill 调试应创建正式 Execution: code=%d body=%#v", code, response)
	}
	execution := response["execution"].(map[string]any)
	if _, exists := execution["workflow_id"]; exists || execution["task_id"] != nil || execution["subtask_id"] != nil {
		t.Fatalf("人工调试不能伪造 Workflow/Task/SubTask 关联: %#v", execution)
	}
	if execution["skill_name"] != "grasp-object" || execution["status"] != "queued" {
		t.Fatalf("Execution 未使用正式 Robot 链路: %#v", execution)
	}

	// request_key 继续复用 Robot Service 的幂等语义，浏览器重试不能重复启动物理动作。
	code, repeated := requestRobotJSON(t, router, http.MethodPost, path, "user-debug", body)
	if code != http.StatusAccepted || repeated["execution"].(map[string]any)["id"] != execution["id"] {
		t.Fatalf("相同人工调试请求必须返回同一 Execution: code=%d body=%#v", code, repeated)
	}

	code, _ = requestRobotJSON(t, router, http.MethodPost, path, "other-user", body)
	if code != http.StatusNotFound {
		t.Fatalf("人工调试不能跨 Project 所有人执行，code=%d", code)
	}
}

func TestDeviceStopRejectsExecutionFromAnotherRobot(t *testing.T) {
	router, st, _, _ := newRobotHandlerTest(t)
	project, _ := st.CreateProject("user", "Project")
	now := time.Now().UTC()
	_ = st.SaveRobotExecution(store.RobotExecution{ID: "rex-other", ProjectID: project.ID, RobotID: "robot-a", PilotInstanceID: "pilot-a",
		SkillName: "skill", SkillVersion: "1", RequestKey: "request", Status: "running", Revision: 1, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now})
	code, _ := requestRobotJSON(t, router, http.MethodPost, "/api/v1/devices/robot-b/stop", "user", []byte(`{"execution_id":"rex-other"}`))
	if code != http.StatusNotFound {
		t.Fatalf("跨 Robot stop 必须拒绝，code=%d", code)
	}
}

func TestDeviceStopRejectsHistoricalInterruptedExecution(t *testing.T) {
	router, st, _, _ := newRobotHandlerTest(t)
	project, _ := st.CreateProject("user-history", "Project")
	now := time.Now().UTC()
	if err := st.SaveRobotExecution(store.RobotExecution{
		ID: "rex-history-interrupted", ProjectID: project.ID, RobotID: "robot-history",
		PilotInstanceID: "pilot-history", SkillName: "grasp-object", SkillVersion: "0.2.0",
		RequestKey: "history", Status: "interrupted", Revision: 2,
		Input: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	code, _ := requestRobotJSON(t, router, http.MethodPost,
		"/api/v1/devices/robot-history/stop", "user-history",
		[]byte(`{"execution_id":"rex-history-interrupted"}`))
	if code != http.StatusConflict {
		t.Fatalf("历史 interrupted 必须明确返回不可停止，code=%d", code)
	}
}

func TestRobotSkillDetailAndResourceHTTPContract(t *testing.T) {
	router, st, _, _ := newRobotHandlerTest(t)
	archive := filepath.Join(t.TempDir(), "grasp-object.zip")
	writeRobotSkillHTTPArchive(t, archive, map[string]string{
		"grasp-object/SKILL.md": `---
name: grasp-object
description: 抓取指定物体并形成 HeldObjectState
category: robot_skill
when_to_use: Robot Task 需要抓取已经解析的目标物体时
version: 0.1.0
required_actions:
  - type: perception.locate_object
    schema_version: 1
stop_actions:
  - type: gripper.hold_object
    schema_version: 1
---
# 抓取物体

通过观测、候选选择和独立验证完成抓取。
`,
		"grasp-object/scripts/skill.py": "async def run(ctx):\n    pass\n",
	})
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: "grasp-object", Version: "0.1.0", Category: "robot_skill",
		PackagePath: archive, PublishedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	code, body := requestRobotJSON(
		t, router, http.MethodGet, "/api/v1/robot-skills/grasp-object/0.1.0", "user", nil,
	)
	skill, ok := body["skill"].(map[string]any)
	if code != http.StatusOK || !ok || skill["body"] == "" {
		t.Fatalf("Robot Skill 详情接口必须返回发布包正文: code=%d body=%#v", code, body)
	}
	resources, ok := skill["resources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("Robot Skill 详情必须返回资源目录: %#v", skill)
	}

	code, body = requestRobotJSON(
		t, router, http.MethodGet,
		"/api/v1/robot-skills/grasp-object/0.1.0/resources/scripts/skill.py", "user", nil,
	)
	resource, ok := body["resource"].(map[string]any)
	if code != http.StatusOK || !ok || resource["path"] != "scripts/skill.py" ||
		body["content"] != "async def run(ctx):\n    pass\n" {
		t.Fatalf("Robot Skill 资源接口必须返回相同发布包内容: code=%d body=%#v", code, body)
	}
}

func writeRobotSkillHTTPArchive(t *testing.T, target string, files map[string]string) {
	t.Helper()
	output, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	for name, content := range files {
		entry, createErr := writer.Create(name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := entry.Write([]byte(content)); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}
