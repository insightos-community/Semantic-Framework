package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
)

// HandleRuntimeInstallations 返回脱敏安装清单；不会把 endpoint、workdir、命令或密钥暴露给浏览器。
func (h *SimulationHandler) HandleRuntimeInstallations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runtime_installations": h.simulation.RuntimeInstallations()})
}

// HandleSceneCatalog 使用 Framework 离线索引，Runtime 未启动时也能浏览。
func (h *SimulationHandler) HandleSceneCatalog(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID != "" {
		if h.access == nil || h.access.ProjectOwnedByUser(
			auth.UserIDFromContext(r.Context()), projectID,
		) != nil {
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
			return
		}
		if _, err := h.simulation.RefreshProjectPublishedSceneCatalog(
			projectID, h.authoring,
		); err != nil && !errors.Is(err, simulation.ErrConflict) {
			h.writeError(w, err)
			return
		}
	}
	profileID := strings.TrimSpace(r.URL.Query().Get("runtime_profile_id"))
	engine := strings.TrimSpace(r.URL.Query().Get("engine"))
	entries := h.simulation.SceneCatalogForProject(projectID, profileID)
	if engine != "" {
		filtered := make([]simulation.SceneCatalogEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.Engine == engine {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"catalog_version": h.simulation.SceneCatalogVersionID(), "scenes": entries,
	})
}

// HandleProjectRuntimePreference 返回可移植 Profile、本机偏好和兼容候选。
func (h *SimulationHandler) HandlePrepareScenePreviews(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	id := chi.URLParam(r, "scene_id")
	visible := false
	for _, entry := range h.simulation.SceneCatalogForProject(chi.URLParam(r, "id"), "") {
		if entry.SceneID == id {
			visible = true
		}
	}
	if !visible {
		writeError(w, http.StatusNotFound, "SCENE_NOT_FOUND", "场景不存在")
		return
	}
	if h.writeError(w, h.simulation.StartScenePreviews(id)) {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}
func (h *SimulationHandler) HandleCancelScenePreviews(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	id := chi.URLParam(r, "scene_id")
	for _, entry := range h.simulation.SceneCatalogForProject(chi.URLParam(r, "id"), "") {
		if entry.SceneID == id {
			h.simulation.CancelScenePreviews(id)
			writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
			return
		}
	}
	writeError(w, http.StatusNotFound, "SCENE_NOT_FOUND", "场景不存在")
}
func (h *SimulationHandler) HandleScenePreviewImage(w http.ResponseWriter, r *http.Request) {
	path, err := h.simulation.ScenePreviewFile(chi.URLParam(r, "key"), chi.URLParam(r, "file"))
	if h.writeError(w, err) {
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, path)
}
func (h *SimulationHandler) HandleBundledScenePreviewImage(w http.ResponseWriter, r *http.Request) {
	path, err := h.simulation.BundledScenePreviewFile(chi.URLParam(r, "scene_id"), r.URL.Query().Get("version"), r.URL.Query().Get("variant"))
	if h.writeError(w, err) {
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, path)
}

func (h *SimulationHandler) HandleProjectRuntimePreference(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	profileID, preferredID, candidates, err := h.simulation.ProjectRuntimePreference(
		chi.URLParam(r, "id"),
	)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runtime_profile_id":                profileID,
		"preferred_runtime_installation_id": preferredID,
		"compatible_runtime_installations":  candidates,
	})
}

func (h *SimulationHandler) HandleSetProjectRuntimePreference(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var request struct {
		RuntimeProfileID               string `json:"runtime_profile_id"`
		PreferredRuntimeInstallationID string `json:"preferred_runtime_installation_id,omitempty"`
	}
	if !decodeJSON(w, r, &request, 1<<20) {
		return
	}
	request.RuntimeProfileID = strings.TrimSpace(request.RuntimeProfileID)
	request.PreferredRuntimeInstallationID = strings.TrimSpace(request.PreferredRuntimeInstallationID)
	if request.RuntimeProfileID == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "runtime_profile_id 不能为空")
		return
	}
	if err := h.simulation.ValidateRuntimePreference(
		request.RuntimeProfileID, request.PreferredRuntimeInstallationID,
	); err != nil {
		h.writeError(w, err)
		return
	}
	project, err := h.projects.SetProjectRuntimePreference(
		auth.UserIDFromContext(r.Context()), chi.URLParam(r, "id"),
		request.RuntimeProfileID, request.PreferredRuntimeInstallationID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "绑定 Runtime 失败")
		return
	}
	h.publish(project.ID, "project", project.ID, project.Revision,
		"simulation.runtime.preference_updated", project)
	writeJSON(w, http.StatusOK, map[string]any{"project": project})
}

// HandleReleaseProjectSimulation 用于显式退出 Project；managed Runtime 会被回收，remote 只解除关联。
func (h *SimulationHandler) HandleReleaseProjectSimulation(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	projectID := chi.URLParam(r, "id")
	if err := h.simulation.ReleaseProject(r.Context(), projectID); h.writeError(w, err) {
		return
	}
	h.publish(projectID, "runtime", "", 1,
		"simulation.project.released", map[string]string{"project_id": projectID})
	writeJSON(w, http.StatusOK, map[string]any{"released": true})
}

// HandleRecoverInterruptedRuntime 只处理 Studio 已明确确认的中断实例清理。
// 请求必须携带最后看到的 instance_id，避免旧页面误清理刚启动的新实例。
func (h *SimulationHandler) HandleRecoverInterruptedRuntime(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var request struct {
		InstanceID string `json:"instance_id"`
	}
	if !decodeJSON(w, r, &request, 1<<20) {
		return
	}
	request.InstanceID = strings.TrimSpace(request.InstanceID)
	if request.InstanceID == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "instance_id 不能为空")
		return
	}
	projectID := chi.URLParam(r, "id")
	info, err := h.simulation.RecoverInterruptedProject(r.Context(), projectID, request.InstanceID)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "runtime", info.RuntimeInstallationID, 1,
		"simulation.runtime.recovered", info)
	writeJSON(w, http.StatusOK, map[string]any{"runtime": info, "instance_cleared": true})
}

func (h *SimulationHandler) HandleListProjectScenes(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	projectID := chi.URLParam(r, "id")
	if _, err := h.simulation.RefreshProjectPublishedSceneCatalog(
		projectID, h.authoring,
	); err != nil && !errors.Is(err, simulation.ErrConflict) {
		h.writeError(w, err)
		return
	}
	references, err := h.projects.ListProjectSceneReferences(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "读取 Project 场景失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_scenes": references, "catalog_scenes": h.simulation.SceneCatalogForProject(projectID, ""),
	})
}

func (h *SimulationHandler) HandleAddProjectScene(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var request struct {
		CatalogSceneID   string `json:"catalog_scene_id"`
		SceneVersion     string `json:"scene_version"`
		DefaultVariantID string `json:"default_variant_id"`
	}
	if !decodeJSON(w, r, &request, 1<<20) {
		return
	}
	request.CatalogSceneID = strings.TrimSpace(request.CatalogSceneID)
	request.SceneVersion = strings.TrimSpace(request.SceneVersion)
	request.DefaultVariantID = strings.TrimSpace(request.DefaultVariantID)
	if request.CatalogSceneID == "" || request.SceneVersion == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "catalog_scene_id 和 scene_version 不能为空")
		return
	}
	projectID := chi.URLParam(r, "id")
	entry, _, variant, err := h.simulation.ValidateProjectCatalogScene(projectID,
		request.CatalogSceneID, request.SceneVersion, request.DefaultVariantID)
	if h.writeError(w, err) {
		return
	}
	project, projectErr := h.projects.GetProject(projectID)
	if projectErr != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "读取 Project 失败")
		return
	}
	if project.RuntimeProfileID == "" {
		project, projectErr = h.projects.SetProjectRuntimePreference(
			auth.UserIDFromContext(r.Context()), projectID,
			entry.CompatibleRuntimeProfile, project.PreferredRuntimeInstallationID,
		)
		if projectErr != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal, "保存 Project Runtime Profile 失败")
			return
		}
	}
	reference, err := h.projects.AddProjectSceneReference(store.ProjectSceneReference{
		ProjectID: projectID, CatalogSceneID: request.CatalogSceneID,
		SceneVersion: request.SceneVersion, DefaultVariantID: variant.VariantID,
	})
	if err != nil {
		writeError(w, http.StatusConflict, "PROJECT_SCENE_EXISTS", "该场景版本已加入 Project")
		return
	}
	h.publish(projectID, "project_scene", reference.ProjectSceneID, 1,
		"simulation.project_scene.added", reference)
	writeJSON(w, http.StatusCreated, map[string]any{"project_scene": reference})
}

func (h *SimulationHandler) HandleRemoveProjectScene(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	projectID, referenceID := chi.URLParam(r, "id"), chi.URLParam(r, "project_scene_id")
	reference, err := h.projects.GetProjectSceneReference(projectID, referenceID)
	if h.writeError(w, err) {
		return
	}
	entry, _, _, err := h.simulation.ValidateProjectCatalogScene(projectID, reference.CatalogSceneID, reference.SceneVersion, reference.DefaultVariantID)
	if h.writeError(w, err) {
		return
	}
	if h.writeError(w, h.simulation.CheckRuntimeInstallIdle(r.Context(), entry.CompatibleRuntimeProfile)) {
		return
	}
	if h.writeError(w, h.projects.RemoveProjectSceneReference(projectID, referenceID)) {
		return
	}
	h.publish(projectID, "project_scene", referenceID, 1, "simulation.project_scene.removed", reference)
	writeJSON(w, http.StatusOK, map[string]any{"removed": referenceID})
}

// HandleCreateProjectLayoutDraft 只允许从 Project 已保存的公共场景引用派生。
// API 不接受完整 SceneDocument，避免普通用户绕过模板和资产目录限制。
func (h *SimulationHandler) HandleCreateProjectLayoutDraft(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var request struct {
		Name            string `json:"name"`
		SourceVariantID string `json:"source_variant_id"`
		Initialization  string `json:"initialization"`
	}
	if !decodeJSON(w, r, &request, 1<<20) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.SourceVariantID = strings.TrimSpace(request.SourceVariantID)
	request.Initialization = strings.TrimSpace(request.Initialization)
	if request.Name == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "name 不能为空")
		return
	}
	projectID := chi.URLParam(r, "id")
	projectSceneID := chi.URLParam(r, "project_scene_id")
	reference, err := h.projects.GetProjectSceneReference(projectID, projectSceneID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "PROJECT_SCENE_NOT_FOUND", "Project 场景不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "读取 Project 场景失败")
		return
	}
	if request.SourceVariantID == "" {
		request.SourceVariantID = reference.DefaultVariantID
	}
	document, err := h.simulation.CreateProjectLayoutDraft(
		h.authoring, projectID, projectSceneID, reference.CatalogSceneID,
		reference.SceneVersion, request.SourceVariantID, request.Initialization,
		request.Name,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_document", document.ID, document.Revision,
		"simulation.layout_draft.created", document)
	writeJSON(w, http.StatusCreated, map[string]any{"scene_document": document})
}

func (h *SimulationHandler) HandleStartProjectScene(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var request struct {
		RequestID             string `json:"request_id"`
		VariantID             string `json:"variant_id"`
		RuntimeInstallationID string `json:"runtime_installation_id,omitempty"`
		Seed                  int64  `json:"seed"`
		Headless              bool   `json:"headless"`
		RenderBackend         string `json:"render_backend"`
	}
	if !decodeJSON(w, r, &request, 1<<20) {
		return
	}
	projectID := chi.URLParam(r, "id")
	reference, err := h.projects.GetProjectSceneReference(projectID,
		chi.URLParam(r, "project_scene_id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "PROJECT_SCENE_NOT_FOUND", "Project 场景不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "读取 Project 场景失败")
		return
	}
	if request.VariantID == "" {
		request.VariantID = reference.DefaultVariantID
	}
	instance, err := h.simulation.StartCatalogScene(r.Context(), projectID,
		reference.CatalogSceneID, simulation.CatalogSceneStartRequest{
			RequestID: request.RequestID, SceneVersion: reference.SceneVersion,
			VariantID: request.VariantID, RuntimeInstallationID: request.RuntimeInstallationID,
			Seed:     request.Seed,
			Headless: request.Headless, RenderBackend: request.RenderBackend,
		})
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_instance", instance.InstanceID, instance.Generation,
		"simulation.scene.started", instance)
	writeJSON(w, http.StatusCreated, map[string]any{"instance": instance})
}

func (h *SimulationHandler) HandleSwitchProjectSceneVariant(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var request struct {
		RequestID string `json:"request_id"`
		VariantID string `json:"variant_id"`
		Seed      int64  `json:"seed"`
	}
	if !decodeJSON(w, r, &request, 1<<20) {
		return
	}
	request.VariantID = strings.TrimSpace(request.VariantID)
	if request.VariantID == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "variant_id 不能为空")
		return
	}
	projectID := chi.URLParam(r, "id")
	instance, err := h.simulation.SwitchCatalogVariant(r.Context(), projectID,
		chi.URLParam(r, "instance_id"), request.VariantID, request.RequestID, request.Seed)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_instance", instance.InstanceID, instance.Generation,
		"simulation.scene.variant_switched", instance)
	writeJSON(w, http.StatusCreated, map[string]any{"instance": instance})
}

func (h *SimulationHandler) HandleTestRuntimeInstallation(w http.ResponseWriter, r *http.Request) {
	installationID := chi.URLParam(r, "installation_id")
	info, err := h.simulation.EnsureRuntimeInstallation(r.Context(), installationID)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime": info})
}

func (h *SimulationHandler) HandleStopRuntimeInstallation(w http.ResponseWriter, r *http.Request) {
	installationID := chi.URLParam(r, "installation_id")
	stopped, err := h.simulation.StopRuntimeInstallation(r.Context(), installationID)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runtime_installation_id": installationID, "managed_process_stopped": stopped,
	})
}

func (h *SimulationHandler) HandleProbeRuntimeInstallation(w http.ResponseWriter, r *http.Request) {
	installationID := chi.URLParam(r, "installation_id")
	info, err := h.simulation.ProbeRuntimeInstallation(r.Context(), installationID)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime": info})
}

// HandleSetRuntimeInstallationEnabled 只切换已登记清单的 enabled 字段。
// Browser 不能提交路径、命令或依赖安装参数；变更后需重启 Server 重新装配 Registry。
func (h *SimulationHandler) HandleSetRuntimeInstallationEnabled(
	w http.ResponseWriter, r *http.Request,
) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body, 1<<10) {
		return
	}
	installationID := chi.URLParam(r, "installation_id")
	view, restartRequired, err := h.simulation.SetRuntimeInstallationEnabled(
		r.Context(), installationID, body.Enabled,
	)
	if h.writeError(w, err) {
		return
	}
	if restartRequired && h.reloadResources != nil {
		if err := h.reloadResources(r.Context()); h.writeError(w, err) {
			return
		}
		restartRequired = false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runtime_installation":    view,
		"server_restart_required": restartRequired,
	})
}
