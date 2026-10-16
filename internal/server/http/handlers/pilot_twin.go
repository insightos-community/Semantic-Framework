package handlers

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/auth"
	"net/http"
)

func (h *RobotsHandler) HandlePilotTwin(w http.ResponseWriter, r *http.Request) {
	pilot := auth.PilotIDFromContext(r.Context())
	if pilot == "" {
		writeError(w, 401, "PILOT_CREDENTIAL_INVALID", "Pilot credential required")
		return
	}
	var value robot.PilotTwinState
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		writeError(w, 400, "INVALID_TWIN_STATE", err.Error())
		return
	}
	if err := h.service.SyncPilotTwin(pilot, value); err != nil {
		writeMapSourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *RobotsHandler) HandleProjectTwin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.projectOwned(r, id) {
		writeError(w, 404, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}
	value, err := h.service.ProjectTwin(auth.UserIDFromContext(r.Context()), id)
	if err != nil {
		writeMapSourceError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, value)
}
