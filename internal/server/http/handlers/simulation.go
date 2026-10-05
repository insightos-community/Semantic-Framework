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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
)

// SimulationAccess 复用 Project 的基本归属和开发模式检查。
type SimulationAccess interface {
	ProjectOwnedByUser(string, string) error
	ProjectWritableByUser(string, string) error
}

type SimulationProjectStore interface {
	SimulationAccess
	GetProject(string) (store.Project, error)
	SetProjectRuntimePreference(string, string, string, string) (store.Project, error)
	AddProjectSceneReference(store.ProjectSceneReference) (store.ProjectSceneReference, error)
	ListProjectSceneReferences(string) ([]store.ProjectSceneReference, error)
	GetProjectSceneReference(string, string) (store.ProjectSceneReference, error)
	RemoveProjectSceneReference(string, string) error
}

// SimulationHandler 是 Studio 仿真工作区的 REST 接入层。
type SimulationHandler struct {
	reloadResources  func(context.Context) error
	uninstallRuntime func(context.Context, string) error
	simulation       *simulation.Service
	authoring        *simulation.SceneAuthoringService
	build            *simulation.SceneBuildService
	projects         SimulationProjectStore
	access           SimulationAccess
	events           projectEventBus
}

func NewSimulationHandler(
	service *simulation.Service,
	authoring *simulation.SceneAuthoringService,
	projects SimulationProjectStore,
	events projectEventBus,
) *SimulationHandler {
	return &SimulationHandler{
		simulation: service, authoring: authoring,
		build:  simulation.NewSceneBuildService(authoring, service),
		access: projects, projects: projects, events: events,
	}
}

func (h *SimulationHandler) HandleEnsureRuntime(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	projectID := chi.URLParam(r, "id")
	info, err := h.simulation.EnsureProjectRuntime(r.Context(), projectID)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "runtime", info.RuntimeInstallationID, 1,
		"simulation.runtime.ready", info)
	writeJSON(w, http.StatusOK, map[string]any{"runtime": info})
}

func (h *SimulationHandler) HandleRuntimeProfiles(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime_profiles": h.simulation.Profiles()})
}

func (h *SimulationHandler) HandleSnapshot(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	projectID := chi.URLParam(r, "id")
	snapshot, err := h.simulation.Snapshot(r.Context(), projectID)
	if h.writeError(w, err) {
		return
	}
	// 完整快照同时包含编辑态和运行态，Studio 重连后不需要猜测两次请求的先后关系。
	snapshot.Documents, err = h.authoring.List(projectID)
	if h.writeError(w, err) {
		return
	}
	snapshot.RuntimeBundles, err = h.build.List(projectID)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"simulation": snapshot})
}

func (h *SimulationHandler) HandleListScenes(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	scenes, err := h.simulation.ListScenes(r.Context())
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scenes": scenes})
}

func (h *SimulationHandler) HandleStartScene(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var request simulation.SceneStartRequest
	if !decodeJSON(w, r, &request, 1<<20) {
		return
	}
	projectID := chi.URLParam(r, "id")
	instance, err := h.simulation.StartScene(
		r.Context(), projectID, chi.URLParam(r, "scene_key"), request,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_instance", instance.InstanceID, 1,
		"simulation.scene.started", instance)
	writeJSON(w, http.StatusCreated, map[string]any{"instance": instance})
}

func (h *SimulationHandler) HandleScene(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	instance, err := h.simulation.Scene(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": instance})
}

func (h *SimulationHandler) HandleSceneOperation(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	operation := chi.URLParam(r, "operation")
	var body any
	if operation == "step" {
		var request struct {
			Steps int `json:"steps"`
		}
		if !decodeJSON(w, r, &request, 1<<20) {
			return
		}
		if request.Steps < 1 || request.Steps > 1000 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "steps 必须在 1 到 1000 之间")
			return
		}
		body = request
	}
	projectID := chi.URLParam(r, "id")
	instance, err := h.simulation.SceneOperation(
		r.Context(), projectID, chi.URLParam(r, "instance_id"), operation, body,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_instance", instance.InstanceID, instance.Generation,
		"simulation.scene."+operation, instance)
	writeJSON(w, http.StatusOK, map[string]any{"instance": instance})
}

func (h *SimulationHandler) HandleSceneSnapshot(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	snapshot, err := h.simulation.SceneSnapshot(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot})
}

func (h *SimulationHandler) HandleViewerScene(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	viewerScene, err := h.simulation.ViewerScene(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewer_scene": viewerScene})
}

func (h *SimulationHandler) HandleViewerSceneContent(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	content, mediaType, err := h.simulation.ViewerSceneContent(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
	)
	if h.writeError(w, err) {
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (h *SimulationHandler) HandleSyncSceneMap(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	projectID := chi.URLParam(r, "id")
	snapshot, err := h.simulation.SyncSceneMap(
		r.Context(), projectID, chi.URLParam(r, "instance_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "synced": true})
}

func (h *SimulationHandler) HandleSourceLink(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	generation, err := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
	if err != nil || generation < 1 {
		writeError(w, http.StatusBadRequest, "SIMULATION_SOURCE_LINK_INVALID",
			"generation 必须是正整数")
		return
	}
	link, err := h.simulation.ResolveSourceLink(
		chi.URLParam(r, "id"), generation,
		r.URL.Query().Get("source_id"), r.URL.Query().Get("entity_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source_link": link})
}

func (h *SimulationHandler) HandleSceneEvaluation(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	evaluation, err := h.simulation.SceneEvaluation(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evaluation": evaluation})
}

func (h *SimulationHandler) HandleRobots(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	robots, err := h.simulation.Robots(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"robots": robots})
}

func (h *SimulationHandler) HandleRobotState(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	state, err := h.simulation.RobotState(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
		chi.URLParam(r, "robot_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state})
}

func (h *SimulationHandler) HandleRobotSensors(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	sensors, err := h.simulation.RobotSensors(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
		chi.URLParam(r, "robot_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sensors": sensors})
}

func (h *SimulationHandler) HandleRobotCommand(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var command simulation.RobotDebugCommand
	if !decodeJSON(w, r, &command, 2<<20) {
		return
	}
	record, err := h.simulation.SubmitRobotCommand(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
		chi.URLParam(r, "robot_id"), command,
	)
	if h.writeError(w, err) {
		return
	}
	projectID := chi.URLParam(r, "id")
	h.publish(projectID, "robot_command", record.CommandID, record.Generation,
		"simulation.robot.command.accepted", record)
	writeJSON(w, http.StatusAccepted, map[string]any{"command": record})
}

func (h *SimulationHandler) HandleRobotCommandState(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	record, err := h.simulation.RobotCommand(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
		chi.URLParam(r, "robot_id"), chi.URLParam(r, "command_id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": record})
}

func (h *SimulationHandler) HandleRobotCommandStop(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	record, err := h.simulation.StopRobotCommand(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
		chi.URLParam(r, "robot_id"), chi.URLParam(r, "command_id"),
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(chi.URLParam(r, "id"), "robot_command", record.CommandID, record.Generation,
		"simulation.robot.command.stopped", record)
	writeJSON(w, http.StatusOK, map[string]any{"command": record})
}

func (h *SimulationHandler) HandleRobotHold(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var body struct {
		Generation int64 `json:"scene_generation"`
	}
	if !decodeJSON(w, r, &body, 1<<20) {
		return
	}
	record, err := h.simulation.HoldRobot(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "instance_id"),
		chi.URLParam(r, "robot_id"), body.Generation,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(chi.URLParam(r, "id"), "robot_command", record.CommandID, record.Generation,
		"simulation.robot.hold", record)
	writeJSON(w, http.StatusOK, map[string]any{"command": record})
}

func (h *SimulationHandler) HandleListSceneAssets(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	catalog := h.authoring.Catalog()
	entries := projectVisualCatalog(chi.URLParam(r, "id"), catalog.Entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version":  catalog.SchemaVersion,
		"catalog_version": catalog.CatalogVersion,
		"assets":          entries,
	})
}

func (h *SimulationHandler) HandleListDocuments(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	documents, err := h.authoring.List(chi.URLParam(r, "id"))
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": documents})
}

func (h *SimulationHandler) HandleCreateDocument(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body, 1<<20) {
		return
	}
	projectID := chi.URLParam(r, "id")
	document, err := h.authoring.Create(projectID, body.Name)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_document", document.ID, document.Revision,
		"simulation.scene.document.created", document)
	writeJSON(w, http.StatusCreated, map[string]any{"document": document})
}

func (h *SimulationHandler) HandleGetDocument(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	document, err := h.authoring.Get(chi.URLParam(r, "id"), chi.URLParam(r, "document_id"))
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": document})
}

func (h *SimulationHandler) HandleUpdateDocument(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var body struct {
		Revision int64                    `json:"revision"`
		Document simulation.SceneDocument `json:"document"`
	}
	if !decodeJSON(w, r, &body, 8<<20) {
		return
	}
	document, err := h.authoring.Update(
		chi.URLParam(r, "id"), chi.URLParam(r, "document_id"), body.Revision, body.Document,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(chi.URLParam(r, "id"), "scene_document", document.ID,
		document.Revision, "simulation.scene.document.updated", document)
	writeJSON(w, http.StatusOK, map[string]any{"document": document})
}

func (h *SimulationHandler) HandleDocumentOperations(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var body struct {
		Revision   int64                       `json:"revision"`
		Operations []simulation.SceneOperation `json:"operations"`
	}
	if !decodeJSON(w, r, &body, 8<<20) {
		return
	}
	document, err := h.authoring.ApplyOperations(
		chi.URLParam(r, "id"), chi.URLParam(r, "document_id"),
		body.Revision, body.Operations,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(chi.URLParam(r, "id"), "scene_document", document.ID,
		document.Revision, "simulation.scene.document.updated", document)
	writeJSON(w, http.StatusOK, map[string]any{"document": document})
}

func (h *SimulationHandler) HandleValidateDocument(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	document, err := h.authoring.Get(chi.URLParam(r, "id"), chi.URLParam(r, "document_id"))
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"validation": h.authoring.ValidateForBuild(document)})
}

func (h *SimulationHandler) HandleBuildDocument(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	bundle, result, err := h.build.Build(
		r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "document_id"), "",
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(chi.URLParam(r, "id"), "runtime_bundle", bundle.RuntimeBundleID,
		bundle.Revision, "simulation.scene.built", result)
	writeJSON(w, http.StatusCreated, map[string]any{
		"runtime_bundle": bundle, "runtime_result": result,
	})
}

func (h *SimulationHandler) HandlePublishDocument(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var body struct {
		Revision int64 `json:"revision"`
	}
	if !decodeJSON(w, r, &body, 1<<20) {
		return
	}
	projectID := chi.URLParam(r, "id")
	document, err := h.authoring.Publish(
		projectID, chi.URLParam(r, "document_id"), body.Revision,
	)
	if h.writeError(w, err) {
		return
	}
	catalogEntries, err := h.simulation.RefreshProjectPublishedSceneCatalog(
		projectID, h.authoring,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_document", document.ID, document.Revision,
		"simulation.scene.document.published", document)
	writeJSON(w, http.StatusOK, map[string]any{"document": document, "catalog_scenes": catalogEntries})
}

func (h *SimulationHandler) HandleForkDocument(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body, 1<<20) {
		return
	}
	projectID := chi.URLParam(r, "id")
	document, err := h.authoring.ForkPublished(
		projectID, chi.URLParam(r, "document_id"), body.Name,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_document", document.ID, document.Revision,
		"simulation.scene.document.forked", document)
	writeJSON(w, http.StatusCreated, map[string]any{"document": document})
}

func (h *SimulationHandler) projectOwned(w http.ResponseWriter, r *http.Request) bool {
	if h.access == nil || h.access.ProjectOwnedByUser(
		auth.UserIDFromContext(r.Context()), chi.URLParam(r, "id"),
	) != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
		return false
	}
	return true
}

func (h *SimulationHandler) projectWritable(w http.ResponseWriter, r *http.Request) bool {
	if !h.projectOwned(w, r) {
		return false
	}
	if h.access.ProjectWritableByUser(
		auth.UserIDFromContext(r.Context()), chi.URLParam(r, "id"),
	) != nil {
		writeError(w, http.StatusConflict, "PROJECT_NOT_WRITABLE", "Project 当前不可写")
		return false
	}
	return true
}

func (h *SimulationHandler) writeError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, simulation.ErrNotFound):
		writeError(w, http.StatusNotFound, "SIMULATION_NOT_FOUND", "仿真资源不存在")
	case errors.Is(err, simulation.ErrConflict):
		writeError(w, http.StatusConflict, "SIMULATION_CONFLICT", err.Error())
	case errors.Is(err, simulation.ErrRuntimeUnavailable):
		writeError(w, http.StatusServiceUnavailable, "SIMULATION_OFFLINE", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
	}
	return true
}

func (h *SimulationHandler) publish(
	projectID, resourceType, resourceID string,
	revision int64,
	eventType string,
	payload any,
) {
	if h.events == nil {
		return
	}
	envelope := ws.NewEnvelope("", ws.ChannelSimulation, eventType, ws.ImportanceNormal, payload)
	envelope.ProjectID = projectID
	envelope.ResourceType = resourceType
	envelope.ResourceID = resourceID
	envelope.Revision = revision
	h.events.Publish(event.TopicAgentEvents, envelope)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不能为空")
		} else {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		}
		return false
	}
	return true
}
