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

package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
)

// RobotsHandler 为设备中心和 Project Robot 时间线提供只读快照与显式操作。
type RobotsHandler struct {
	service *robotdomain.Service
	st      *store.Store
}

func NewRobotsHandler(service *robotdomain.Service, st *store.Store) *RobotsHandler {
	return &RobotsHandler{service: service, st: st}
}

// HandlePilotTransfer 复用 Robot Service 的流式传输实现。认证由主 HTTP
// Router 的统一中间件完成，因此 Skill 包和 Artifact 不需要再占用 Pilot
// 控制 WebSocket，也不另建文件服务。
func (h *RobotsHandler) HandleCreatePilotEnrollment(w http.ResponseWriter, r *http.Request) {
	item, err := h.service.CreatePilotEnrollment(auth.UserIDFromContext(r.Context()))
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"enrollment": item})
}

func (h *RobotsHandler) HandleClaimPilotEnrollment(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JoinCode string `json:"join_code"`
		PilotID  string `json:"pilot_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "join_code 和 pilot_id 必填")
		return
	}
	item, credential, err := h.service.ClaimPilotEnrollment(request.JoinCode, request.PilotID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, "PILOT_ENROLLMENT_INVALID", "加入码不存在、已使用或已过期")
		} else {
			writeRobotError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrollment": item, "pilot_id": request.PilotID,
		"credential": credential, "websocket_path": "/ws/pilot"})
}

func (h *RobotsHandler) HandleRevokePilotEnrollment(w http.ResponseWriter, r *http.Request) {
	item, err := h.st.GetPilotEnrollment(chi.URLParam(r, "id"))
	if err != nil || item.CreatedBy != auth.UserIDFromContext(r.Context()) {
		writeError(w, http.StatusNotFound, "PILOT_ENROLLMENT_NOT_FOUND", "Pilot 加入码不存在")
		return
	}
	if err := h.service.RevokePilotEnrollment(item.ID); err != nil {
		writeRobotError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *RobotsHandler) HandlePilotTransfer(w http.ResponseWriter, r *http.Request) {
	h.service.TransferHandler(w, r, auth.PilotIDFromContext(r.Context()))
}

func (h *RobotsHandler) HandleListDevices(w http.ResponseWriter, r *http.Request) {
	snapshot, err := h.service.DeviceSnapshot()
	if err != nil {
		writeError(w, 500, "ROBOT_LIST_FAILED", err.Error())
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	backend := strings.TrimSpace(r.URL.Query().Get("backend"))
	filtered := make([]map[string]any, 0, len(snapshot.Robots))
	for _, item := range snapshot.Robots {
		// 列表、详情和 WebSocket 快照必须使用同一份在线事实。数据库中的
		// RobotPilot 只是最后一次上报，不能覆盖当前 Gateway 会话与最新的
		// 受管 Runtime 状态，否则页面会同时显示在线、离线和 starting。
		itemStatus, _ := item["status"].(string)
		if status != "" && itemStatus != status {
			continue
		}
		itemModel, _ := item["model"].(string)
		if model != "" && itemModel != model {
			continue
		}
		itemBackend, _ := item["backend"].(string)
		if backend != "" && itemBackend != backend {
			continue
		}
		filtered = append(filtered, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": filtered})
}

func (h *RobotsHandler) HandleGetDevice(w http.ResponseWriter, r *http.Request) {
	robotID := chi.URLParam(r, "robot_id")
	device, err := h.service.Device(robotID)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	executions, err := h.st.ListRobotExecutions("", robotID, 50)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	packages, err := h.st.ListRobotSkillPackages()
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": device, "executions": executions, "skill_packages": packages})
}

func (h *RobotsHandler) HandleDeviceSnapshot(w http.ResponseWriter, _ *http.Request) {
	snapshot, err := h.service.DeviceSnapshot()
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot})
}

func (h *RobotsHandler) HandleListExecutions(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	robotID := chi.URLParam(r, "robot_id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if projectID != "" && !h.projectOwned(r, projectID) {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
		return
	}
	items, err := h.st.ListRobotExecutions(projectID, robotID, limit)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"executions": items})
}

func (h *RobotsHandler) HandleListProjectExecutions(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	if !h.projectOwned(r, projectID) {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
		return
	}
	items, err := h.st.ListRobotExecutions(projectID, "", 200)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"executions": items})
}

// HandleStartSkillDebug 是 Studio 中“人工调试 Robot Skill”的产品入口。
// 它复用正式 Robot Execution、Pilot 和 stop 链路，但不伪造 Workflow、Task
// 或 SubTask。Agent 执行仍由 robot.run 的 Task scope 校验保护；此入口只接受
// 已登录用户对自己 Project 的显式操作，因此两条路径不会混用权限语义。
func (h *RobotsHandler) HandleStartSkillDebug(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	if !h.projectOwned(r, projectID) {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
		return
	}
	var request struct {
		SkillName    string         `json:"skill_name"`
		SkillVersion string         `json:"skill_version"`
		Input        map[string]any `json:"input"`
		RequestKey   string         `json:"request_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
		strings.TrimSpace(request.SkillName) == "" ||
		strings.TrimSpace(request.SkillVersion) == "" ||
		strings.TrimSpace(request.RequestKey) == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "skill_name、skill_version 和 request_key 必填")
		return
	}
	if request.Input == nil {
		request.Input = map[string]any{}
	}
	execution, err := h.service.Run(r.Context(), robotdomain.RunRequest{
		ProjectID:    projectID,
		RobotID:      chi.URLParam(r, "robot_id"),
		SkillName:    strings.TrimSpace(request.SkillName),
		SkillVersion: strings.TrimSpace(request.SkillVersion),
		RequestKey:   strings.TrimSpace(request.RequestKey),
		Input:        request.Input,
	})
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"execution": execution})
}

func (h *RobotsHandler) HandleGetExecution(w http.ResponseWriter, r *http.Request) {
	item, err := h.st.GetRobotExecution(chi.URLParam(r, "execution_id"))
	if err != nil || !h.projectOwned(r, item.ProjectID) {
		writeError(w, http.StatusNotFound, "ROBOT_EXECUTION_NOT_FOUND", "Robot Execution 不存在")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after_sequence"), 10, 64)
	// 多阶段导航会产生数百条Feedback/Progress。详情接口按sequence分页，
	// 不能静默截断在500条后让Studio误以为后续Stage从未发生。
	events, err := h.st.ListRobotExecutionEvents(item.ID, after, 501)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	hasMore := len(events) > 500
	if hasMore {
		events = events[:500]
	}
	nextSequence := after
	if len(events) > 0 {
		nextSequence = events[len(events)-1].Sequence
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"execution": item, "events": events,
		"has_more": hasMore, "next_sequence": nextSequence,
	})
}

func (h *RobotsHandler) HandleStopExecution(w http.ResponseWriter, r *http.Request) {
	item, err := h.st.GetRobotExecution(chi.URLParam(r, "execution_id"))
	if err != nil || !h.projectOwned(r, item.ProjectID) {
		writeError(w, http.StatusNotFound, "ROBOT_EXECUTION_NOT_FOUND", "Robot Execution 不存在")
		return
	}
	var request struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	stopped, err := h.service.Stop(r.Context(), item.ProjectID, item.ID, request.Reason)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"execution": stopped})
}

func (h *RobotsHandler) HandleStopDevice(w http.ResponseWriter, r *http.Request) {
	robotID := chi.URLParam(r, "robot_id")
	var request struct {
		ExecutionID string `json:"execution_id"`
		Reason      string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ExecutionID == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "execution_id 必填")
		return
	}
	item, err := h.st.GetRobotExecution(request.ExecutionID)
	if err != nil || item.RobotID != robotID || !h.projectOwned(r, item.ProjectID) {
		writeError(w, http.StatusNotFound, "ROBOT_EXECUTION_NOT_FOUND", "Robot Execution 不存在")
		return
	}
	stopped, stopErr := h.service.Stop(r.Context(), item.ProjectID, item.ID, request.Reason)
	if stopErr != nil {
		writeRobotError(w, stopErr)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "execution": stopped})
}

func (h *RobotsHandler) HandleAgentReply(w http.ResponseWriter, r *http.Request) {
	item, err := h.st.GetRobotExecution(chi.URLParam(r, "execution_id"))
	if err != nil || !h.projectOwned(r, item.ProjectID) {
		writeError(w, http.StatusNotFound, "ROBOT_EXECUTION_NOT_FOUND", "Robot Execution 不存在")
		return
	}
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, 400, "BAD_REQUEST", err.Error())
		return
	}
	if err := h.service.ReplyAgentRequest(item.ID, payload); err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func (h *RobotsHandler) HandleListRobotSkills(w http.ResponseWriter, _ *http.Request) {
	items, err := h.st.ListRobotSkillPackages()
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": items})
}

// HandleGetRobotSkill 返回发布包中的 SKILL.md 和资源目录。Robot Skill 与
// Agent Skill 使用相同的文档查看体验，但安装状态仍由具体 Pilot 独立管理。
func (h *RobotsHandler) HandleGetRobotSkill(w http.ResponseWriter, r *http.Request) {
	detail, err := h.service.GetSkillPackageDetail(chi.URLParam(r, "name"), chi.URLParam(r, "version"))
	if err != nil {
		writeRobotSkillReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": detail})
}

func (h *RobotsHandler) HandleGetRobotSkillResource(w http.ResponseWriter, r *http.Request) {
	resource, content, err := h.service.ReadSkillPackageResource(
		chi.URLParam(r, "name"), chi.URLParam(r, "version"), chi.URLParam(r, "*"),
	)
	if err != nil {
		writeRobotSkillReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resource": resource, "content": content})
}

func writeRobotSkillReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "ROBOT_SKILL_NOT_FOUND", err.Error())
	case errors.Is(err, robotdomain.ErrSkillResourceInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_RESOURCE_PATH", err.Error())
	case errors.Is(err, robotdomain.ErrSkillResourceTooLarge), errors.Is(err, robotdomain.ErrSkillResourceNotText):
		writeError(w, http.StatusUnprocessableEntity, "RESOURCE_NOT_PREVIEWABLE", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "ROBOT_SKILL_READ_FAILED", err.Error())
	}
}

func (h *RobotsHandler) HandlePublishRobotSkill(w http.ResponseWriter, r *http.Request) {
	item, err := h.service.PublishSkillArchive(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "ROBOT_SKILL_INVALID", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"skill": item})
}

func (h *RobotsHandler) HandleInstallRobotSkill(w http.ResponseWriter, r *http.Request) {
	robotID := chi.URLParam(r, "robot_id")
	name, version := chi.URLParam(r, "name"), chi.URLParam(r, "version")
	desired, err := h.service.SetDesiredSkill(robotID, name, version, true)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	device, _ := h.service.Device(robotID)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "desired_skill": desired, "robot": device})
}

func (h *RobotsHandler) HandleRobotSkillAction(w http.ResponseWriter, r *http.Request) {
	robotID := chi.URLParam(r, "robot_id")
	name, version, action := chi.URLParam(r, "name"), chi.URLParam(r, "version"), chi.URLParam(r, "action")
	var desired store.RobotDesiredSkill
	var err error
	switch action {
	case "enable":
		desired, err = h.service.SetDesiredSkill(robotID, name, version, true)
	case "disable":
		desired, err = h.service.SetDesiredSkill(robotID, name, version, false)
	case "uninstall":
		err = h.service.RemoveDesiredSkill(robotID, name)
	default:
		writeError(w, 404, "ROBOT_SKILL_ACTION_UNKNOWN", "未知 Robot Skill 操作")
		return
	}
	if err != nil {
		writeRobotError(w, err)
		return
	}
	device, _ := h.service.Device(robotID)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "desired_skill": desired, "robot": device})
}

func (h *RobotsHandler) HandleDeleteRobotSkill(w http.ResponseWriter, r *http.Request) {
	robotID := chi.URLParam(r, "robot_id")
	name, _ := chi.URLParam(r, "name"), chi.URLParam(r, "version")
	if err := h.service.RemoveDesiredSkill(robotID, name); err != nil {
		writeRobotError(w, err)
		return
	}
	device, _ := h.service.Device(robotID)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "removed": true, "robot": device})
}

func (h *RobotsHandler) HandleStartAbilityDebug(w http.ResponseWriter, r *http.Request) {
	robotID := chi.URLParam(r, "robot_id")
	var request struct {
		AbilityInstanceID string         `json:"ability_instance_id"`
		TaskName          string         `json:"task_name"`
		Input             map[string]any `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.AbilityInstanceID == "" || request.TaskName == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "ability_instance_id 和 task_name 必填")
		return
	}
	debug, err := h.service.StartAbilityDebug(robotID, request.AbilityInstanceID, request.TaskName, request.Input)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"debug_execution": debug})
}

func (h *RobotsHandler) HandleStopAbilityDebug(w http.ResponseWriter, r *http.Request) {
	debug, err := h.service.StopAbilityDebug(chi.URLParam(r, "robot_id"), chi.URLParam(r, "debug_id"))
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"debug_execution": debug})
}

func (h *RobotsHandler) projectOwned(r *http.Request, projectID string) bool {
	project, err := h.st.GetProject(projectID)
	return err == nil && project.OwnerID == auth.UserIDFromContext(r.Context())
}

func writeRobotError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "ROBOT_NOT_FOUND", err.Error())
	case errors.Is(err, robotdomain.ErrPilotOffline):
		writeError(w, 503, "PILOT_OFFLINE", err.Error())
	case errors.Is(err, robotdomain.ErrRobotBusy):
		writeError(w, 409, "ROBOT_BUSY", err.Error())
	case errors.Is(err, robotdomain.ErrExecutionNotActive):
		writeError(w, 409, "ROBOT_EXECUTION_NOT_ACTIVE", err.Error())
	case errors.Is(err, robotdomain.ErrSkillUnavailable):
		writeError(w, 409, "ROBOT_SKILL_UNAVAILABLE", err.Error())
	case errors.Is(err, robotdomain.ErrAbilityTaskUnknown):
		writeError(w, 409, "ABILITY_TASK_UNAVAILABLE", err.Error())
	case errors.Is(err, robotdomain.ErrRequestConflict):
		writeError(w, 409, "ROBOT_REQUEST_CONFLICT", err.Error())
	default:
		writeError(w, 500, "ROBOT_OPERATION_FAILED", err.Error())
	}
}
