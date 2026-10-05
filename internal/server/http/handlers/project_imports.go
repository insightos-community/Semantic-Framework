package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"insightos.cn/semantic-framework/internal/install"
)

func (h *ProjectsHandler) SetImports(inbox *install.Inbox) { h.imports = inbox }

// 导入入口只接受当前用户项目中的包内容，服务器落点由项目工作区决定。
// Web 与 CLI 都不能通过请求参数指定任意服务器目录。
func (h *ProjectsHandler) importProject(w http.ResponseWriter, r *http.Request, writing bool) (string, bool) {
	p, ok := h.ownedProject(w, r)
	if !ok {
		return "", false
	}
	if writing && p.Mode != "development" {
		writeError(w, http.StatusConflict, "PROJECT_MODE", "请切换到开发模式后导入")
		return "", false
	}
	if h.imports == nil {
		writeError(w, http.StatusServiceUnavailable, "IMPORT_UNAVAILABLE", "项目导入服务尚未启用")
		return "", false
	}
	return p.ID, true
}

func (h *ProjectsHandler) HandleListImports(w http.ResponseWriter, r *http.Request) {
	id, ok := h.importProject(w, r, false)
	if !ok {
		return
	}
	items, err := h.imports.List(id)
	if err != nil {
		h.internalError(w, "读取导入记录失败", err)
		return
	}
	directory, err := h.imports.Directory(id)
	if err != nil {
		h.internalError(w, "读取导入目录失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"directory": directory, "items": items})
}

func (h *ProjectsHandler) HandleUploadImport(w http.ResponseWriter, r *http.Request) {
	id, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	// 权重包采用流式上传，读取时限独立于短 JSON 请求，避免正常大包被全局
	// ReadTimeout 提前截断；大小上限和项目身份校验仍保留。
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Minute))
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, install.MaxUploadBytes)
	item, err := h.imports.UploadReader(r.Context(), id, r.URL.Query().Get("filename"), r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	// 包解析错误保留为记录，客户端统一展示该记录的失败原因及重试入口。
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

func (h *ProjectsHandler) HandleInstallImport(w http.ResponseWriter, r *http.Request) {
	id, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	var options install.Options
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&options); err != nil {
		writeError(w, 400, CodeBadRequest, err.Error())
		return
	}
	item, err := h.imports.Install(id, chi.URLParam(r, "import_id"), options)
	if err != nil {
		writeError(w, 409, "INSTALL_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"item": item})
}

func (h *ProjectsHandler) HandleScanImports(w http.ResponseWriter, r *http.Request) {
	id, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	if err := h.imports.Scan(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	h.HandleListImports(w, r)
}

func (h *ProjectsHandler) HandleRetryImport(w http.ResponseWriter, r *http.Request) {
	id, ok := h.importProject(w, r, true)
	if !ok {
		return
	}
	item, err := h.imports.Retry(r.Context(), id, chi.URLParam(r, "import_id"))
	if err != nil {
		writeError(w, http.StatusConflict, "IMPORT_RETRY_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}
