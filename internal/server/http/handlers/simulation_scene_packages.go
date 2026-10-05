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
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// HandleListDocumentLayouts 返回同一逻辑场景的全部 Layout 草稿和发布历史。
func (h *SimulationHandler) HandleListDocumentLayouts(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	projectID := chi.URLParam(r, "id")
	document, err := h.authoring.Get(projectID, chi.URLParam(r, "document_id"))
	if h.writeError(w, err) {
		return
	}
	layouts, err := h.authoring.ListLayouts(projectID, document.SceneID)
	if h.writeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scene_id": document.SceneID, "layouts": layouts,
	})
}

// HandleCreateDocumentLayout 复制现有 Layout 为独立草稿；运行时仍分别构建 Bundle。
func (h *SimulationHandler) HandleCreateDocumentLayout(w http.ResponseWriter, r *http.Request) {
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
	document, err := h.authoring.CreateLayout(
		projectID, chi.URLParam(r, "document_id"), body.Name,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_document", document.ID, document.Revision,
		"simulation.scene.layout.created", document)
	writeJSON(w, http.StatusCreated, map[string]any{"document": document})
}

func (h *SimulationHandler) HandleRenameDocumentLayout(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	var body struct {
		Revision int64  `json:"revision"`
		Name     string `json:"name"`
	}
	if !decodeJSON(w, r, &body, 1<<20) {
		return
	}
	projectID := chi.URLParam(r, "id")
	document, err := h.authoring.RenameLayout(
		projectID, chi.URLParam(r, "document_id"), body.Revision, body.Name,
	)
	if h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_document", document.ID, document.Revision,
		"simulation.scene.layout.renamed", document)
	writeJSON(w, http.StatusOK, map[string]any{"document": document})
}

func (h *SimulationHandler) HandleDeleteDocumentLayout(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	projectID := chi.URLParam(r, "id")
	document, err := h.authoring.Get(projectID, chi.URLParam(r, "document_id"))
	if h.writeError(w, err) {
		return
	}
	if err := h.authoring.DeleteLayout(projectID, document.ID); h.writeError(w, err) {
		return
	}
	h.publish(projectID, "scene_document", document.ID, document.Revision,
		"simulation.scene.layout.deleted", map[string]string{
			"document_id": document.ID, "scene_id": document.SceneID,
			"layout_id": document.LayoutID,
		})
	w.WriteHeader(http.StatusNoContent)
}

// HandleExportScenePackage 只导出已发布 Layout 和资产版本引用。
func (h *SimulationHandler) HandleExportScenePackage(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	data, filename, err := h.authoring.ExportScenePackage(
		chi.URLParam(r, "id"), chi.URLParam(r, "scene_id"),
	)
	if h.writeError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`,
		strings.ReplaceAll(filename, `"`, "")))
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// HandleImportScenePackage 不解压到宿主路径，也不接受脚本。全部 Layout 校验通过后
// 才写入 Project 草稿目录。
func (h *SimulationHandler) HandleImportScenePackage(w http.ResponseWriter, r *http.Request) {
	if !h.projectWritable(w, r) {
		return
	}
	projectID := chi.URLParam(r, "id")
	profile, err := h.simulation.ProjectRuntimeProfile(projectID)
	if h.writeError(w, err) {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (64<<20)+1))
	if err != nil || len(data) > 64<<20 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "Scene Package 读取失败或超过 64 MiB")
		return
	}
	documents, err := h.authoring.ImportScenePackage(
		projectID, profile.RuntimeProfileID, data,
	)
	if h.writeError(w, err) {
		return
	}
	for _, document := range documents {
		h.publish(projectID, "scene_document", document.ID, document.Revision,
			"simulation.scene.package.imported", document)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"documents": documents})
}
