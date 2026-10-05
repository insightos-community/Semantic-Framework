package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"insightos.cn/semantic-framework/internal/install"
)

func (h *ProjectsHandler) SetComponents(components *install.ComponentStore, checks ...func(context.Context, install.InstalledComponent) error) {
	h.components = components
	if len(checks) > 0 {
		h.componentRemoval = checks[0]
	}
}

func (h *ProjectsHandler) SetComponentApply(apply func(context.Context, string, string) error) {
	h.componentApply = apply
}

func componentImported(records []install.Record, item install.InstalledComponent) bool {
	for _, record := range records {
		if record.ID == item.ID {
			return true
		}
		for _, resource := range record.Resources {
			if resource == item.Name+"@"+item.Version {
				return true
			}
		}
	}
	return false
}

func (h *ProjectsHandler) HandleRemoveComponent(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	if h.components == nil || h.componentRemoval == nil {
		writeError(w, 503, "INSTALL_UNAVAILABLE", "组件卸载未启用")
		return
	}
	id := chi.URLParam(r, "component_id")
	item, err := h.components.Get(id)
	if err != nil {
		writeError(w, 404, "COMPONENT_NOT_FOUND", "未找到已安装组件")
		return
	}
	records, err := h.imports.List(projectID)
	if err != nil {
		h.internalError(w, "读取导入记录失败", err)
		return
	}
	if !componentImported(records, item) {
		writeError(w, 403, "COMPONENT_PROJECT", "组件未由当前项目导入")
		return
	}
	if err := h.components.Remove(r.Context(), id, h.componentRemoval); err != nil {
		writeError(w, 409, "COMPONENT_IN_USE", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"removed": id, "message": "已卸载组件，导入原包与历史记录保留"})
}

func (h *ProjectsHandler) HandleComponentVersions(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.importProject(w, r, false)
	if !ok {
		return
	}
	if h.components == nil {
		writeError(w, 503, "INSTALL_UNAVAILABLE", "组件安装未启用")
		return
	}
	instances, err := h.st.ListRuntimeInstances(r.Context())
	if err != nil {
		h.internalError(w, "读取 Robot 安装版本失败", err)
		return
	}
	robots := []map[string]any{}
	seen := map[string]bool{}
	for _, instance := range instances {
		if instance.ProjectID != projectID || seen[instance.RobotID] {
			continue
		}
		seen[instance.RobotID] = true
		item := map[string]any{"robot_id": instance.RobotID, "robot_model": instance.RobotModel, "backend_profile": instance.BackendProfile, "status": instance.Status, "bundle_version": instance.BundleVersion, "failure_reason": instance.FailureReason}
		for key, path := range map[string]string{"desired": install.BindingPath(h.components.Root, instance.RobotID), "running": filepath.Join(instance.DataDirectory, "run", "components.json")} {
			body, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				h.internalError(w, "读取组件绑定失败", err)
				return
			}
			var binding install.RobotBinding
			if err := json.Unmarshal(body, &binding); err != nil {
				h.internalError(w, "解析组件绑定失败", err)
				return
			}
			// 已退出实例的快照属于上次执行，不能宣称它仍在运行。
			if key == "running" && !instance.Status.Active() {
				key = "last_execution"
			}
			item[key] = binding
		}
		robots = append(robots, item)
	}
	items, err := h.components.List()
	if err != nil {
		h.internalError(w, "读取已安装组件失败", err)
		return
	}
	records, err := h.imports.List(projectID)
	if err != nil {
		h.internalError(w, "读取导入记录失败", err)
		return
	}
	installed := []install.InstalledComponent{}
	for _, item := range items {
		if componentImported(records, item) {
			installed = append(installed, item)
		}
	}
	// 安装库是本机共享资源，项目可以复用已有制品；卸载权限仍按原导入归属检查。
	// 展示默认组合，便于场景首次启动前选好型号、Ability 和模型。
	defaults := map[string]install.RobotBinding{}
	for _, item := range items {
		for _, model := range item.RobotModels {
			binding, err := h.components.ProjectDefault(projectID, model)
			if err != nil {
				h.internalError(w, "读取项目默认组件失败", err)
				return
			}
			if binding != nil {
				defaults[model] = *binding
			}
		}
	}
	project, err := h.st.GetProject(projectID)
	if err != nil {
		h.internalError(w, "读取项目失败", err)
		return
	}
	writeJSON(w, 200, map[string]any{"robots": robots, "installed": installed, "available": items, "defaults": defaults, "runtime_profile": project.RuntimeProfileID})
}

// 绑定选择与安装分开：请求只接收已安装组件 ID，路径及 Python 环境均从收据读取。
// 保存成功不代表正在运行的模型已切换，生效由独立入口执行原有安全停止与就绪检查。
func (h *ProjectsHandler) HandleBindComponents(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	if h.components == nil {
		writeError(w, 503, "INSTALL_UNAVAILABLE", "组件安装未启用")
		return
	}
	var input struct {
		RobotID      string   `json:"robot_id"`
		RobotModel   string   `json:"robot_model"`
		ComponentIDs []string `json:"component_ids"`
	}
	if !decodeJSON(w, r, &input, 1<<20) {
		return
	}
	var binding install.RobotBinding
	var err error
	if input.RobotID != "" {
		instance, e := h.st.GetLatestRuntimeByRobot(r.Context(), input.RobotID)
		if e != nil || instance.ProjectID != projectID {
			writeError(w, 404, "ROBOT_NOT_FOUND", "当前项目中没有该受管 Robot")
			return
		}
		// 型号和后端取实际设备，客户端不能靠伪造型号绕过兼容性检查。
		binding, err = h.components.SelectBinding(input.ComponentIDs, input.RobotID, instance.RobotModel, instance.BackendProfile)
	} else {
		project, e := h.st.GetProject(projectID)
		if e != nil {
			h.internalError(w, "读取项目失败", e)
			return
		}
		binding, err = h.components.SelectProjectDefault(input.ComponentIDs, projectID, input.RobotModel, project.RuntimeProfileID)
	}
	if err != nil {
		writeError(w, 409, "BINDING_INCOMPATIBLE", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"binding": binding, "message": "已保存；现有 Robot 下次启动时生效，项目默认用于后续首次绑定"})
}

func (h *ProjectsHandler) HandleApplyComponents(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	if h.componentApply == nil {
		writeError(w, 503, "INSTALL_UNAVAILABLE", "受管 Robot 生效未启用")
		return
	}
	var input struct {
		RobotID string `json:"robot_id"`
	}
	if !decodeJSON(w, r, &input, 1<<20) {
		return
	}
	instance, err := h.st.GetLatestRuntimeByRobot(r.Context(), input.RobotID)
	if err != nil || instance.ProjectID != projectID {
		writeError(w, 404, "ROBOT_NOT_FOUND", "当前项目中没有该受管 Robot")
		return
	}
	if err := h.componentApply(r.Context(), projectID, input.RobotID); err != nil {
		writeError(w, 409, "COMPONENT_APPLY_FAILED", fmt.Sprintf("绑定保留，生效未完成：%s", err))
		return
	}
	writeJSON(w, 200, map[string]any{"message": "目标 Robot 已通过就绪检查，绑定已生效"})
}

func (h *ProjectsHandler) HandleRollbackComponents(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	if h.components == nil {
		writeError(w, 503, "INSTALL_UNAVAILABLE", "组件安装未启用")
		return
	}
	var input struct {
		RobotID  string `json:"robot_id"`
		Revision string `json:"revision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, 400, CodeBadRequest, err.Error())
		return
	}
	instance, err := h.st.GetLatestRuntimeByRobot(r.Context(), input.RobotID)
	if err != nil || instance.ProjectID != projectID {
		writeError(w, 404, "ROBOT_NOT_FOUND", "当前项目中没有该受管 Robot")
		return
	}
	binding, err := h.components.Rollback(input.RobotID, input.Revision)
	if err != nil {
		writeError(w, 409, "ROLLBACK_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"binding": binding, "message": "绑定已回退，下次启动目标 Robot 时生效"})
}

func (h *ProjectsHandler) HandleCancelInstallation(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	if err := h.imports.CancelInstall(projectID, chi.URLParam(r, "import_id")); err != nil {
		writeError(w, 409, "INSTALL_CANCEL_FAILED", err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"message": "正在取消安装，已有运行版本保持不变"})
}
