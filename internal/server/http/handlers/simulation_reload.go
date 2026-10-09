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
	"github.com/go-chi/chi/v5"
	"net/http"
)

func (h *SimulationHandler) SetResourceReload(reload func(context.Context) error, uninstall ...func(context.Context, string) error) {
	h.reloadResources = reload
	if len(uninstall) > 0 {
		h.uninstallRuntime = uninstall[0]
	}
}

func (h *SimulationHandler) HandleUninstallRuntime(w http.ResponseWriter, r *http.Request) {
	if h.uninstallRuntime == nil {
		writeError(w, 503, "INSTALL_UNAVAILABLE", "Runtime 卸载尚未装配")
		return
	}
	if err := h.uninstallRuntime(r.Context(), chi.URLParam(r, "installation_id")); err != nil {
		writeError(w, 409, "RUNTIME_IN_USE", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"uninstalled": true})
}

func (h *SimulationHandler) HandleReloadResources(w http.ResponseWriter, r *http.Request) {
	if h.reloadResources == nil {
		writeError(w, 503, "INSTALL_UNAVAILABLE", "安装目录刷新尚未装配")
		return
	}
	if err := h.reloadResources(r.Context()); err != nil {
		writeError(w, 409, "INSTALL_RELOAD_FAILED", err.Error())
		return
	}
	h.HandleRuntimeInstallations(w, r)
}
