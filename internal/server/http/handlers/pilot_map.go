package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"io"
	"net/http"
	"time"
)

func (h *RobotsHandler) HandlePilotMap(w http.ResponseWriter, r *http.Request) {
	pilotID := auth.PilotIDFromContext(r.Context())
	if pilotID == "" {
		writeError(w, http.StatusUnauthorized, "PILOT_CREDENTIAL_INVALID", "Pilot credential required")
		return
	}
	var body struct {
		ProjectID string                 `json:"project_id"`
		Snapshot  store.PilotMapSnapshot `json:"snapshot"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, 400, "INVALID_MAP", err.Error())
		return
	}
	result, err := h.service.SyncPilotMap(pilotID, body.ProjectID, body.Snapshot)
	if err != nil {
		writeMapSourceError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (h *RobotsHandler) HandleMapSources(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	if !h.projectOwned(r, projectID) {
		writeError(w, 404, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}
	result, err := h.service.MapSources(auth.UserIDFromContext(r.Context()), projectID)
	if err != nil {
		writeMapSourceError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (h *RobotsHandler) HandleSelectMapSource(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	if !h.projectOwned(r, projectID) {
		writeError(w, 404, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}
	var body struct {
		PilotID *string `json:"pilot_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.PilotID == nil {
		writeError(w, 400, "INVALID_MAP_SOURCE", "pilot_id 必填；空字符串表示停止同步")
		return
	}
	result, err := h.service.SelectMapSource(auth.UserIDFromContext(r.Context()), projectID, *body.PilotID)
	if err != nil {
		writeMapSourceError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"map_snapshot": publicMapSnapshot(result)})
}

func writeMapSourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, robotdomain.ErrDirectRunScope):
		writeError(w, 403, "MAP_SOURCE_FORBIDDEN", "无权使用该项目或 Pilot 地图来源")
	case errors.Is(err, robotdomain.ErrMapSourceUnavailable):
		writeError(w, 409, "MAP_SOURCE_UNAVAILABLE", err.Error())
	case errors.Is(err, store.ErrInvalidState):
		writeError(w, 400, "INVALID_MAP", err.Error())
	case errors.Is(err, store.ErrRevisionConflict), errors.Is(err, store.ErrStaleMapGeneration):
		writeError(w, 409, "MAP_REVISION_CONFLICT", err.Error())
	default:
		writeRobotError(w, err)
	}
}

func (h *RobotsHandler) HandlePilotMapAsset(w http.ResponseWriter, r *http.Request) {
	pilotID := auth.PilotIDFromContext(r.Context())
	if err := h.service.AuthorizeMapAsset(pilotID); err != nil {
		writeMapSourceError(w, err)
		return
	}
	hash := chi.URLParam(r, "hash")
	if !store.ValidMapAssetHash(hash) {
		writeMapSourceError(w, store.ErrInvalidState)
		return
	}
	if r.Method == http.MethodHead {
		_, err := h.st.PilotMapAssetSize(pilotID, hash)
		if err != nil {
			writeMapSourceError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, store.MaxPilotMapAssetBytes))
	if err != nil {
		writeMapSourceError(w, store.ErrInvalidState)
		return
	}
	if err = h.st.PutPilotMapAsset(pilotID, hash, content); err != nil {
		writeMapSourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *RobotsHandler) HandleProjectMapAsset(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	if !h.projectOwned(r, projectID) {
		writeError(w, 404, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}
	hash := chi.URLParam(r, "hash")
	if !store.ValidMapAssetHash(hash) {
		writeMapSourceError(w, store.ErrInvalidState)
		return
	}
	content, err := h.st.ProjectMapAsset(projectID, hash)
	if err != nil {
		writeMapSourceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "model/gltf-binary")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", "\""+hash+"\"")
	http.ServeContent(w, r, hash+".glb", time.Time{}, bytes.NewReader(content))
}
