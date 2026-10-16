package handlers

import (
	"github.com/go-chi/chi/v5"
	"insightos.cn/semantic-framework/internal/server/auth"
	"net/http"
)

func (h *RobotsHandler) HandleProjectRobots(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	if !h.projectOwned(r, projectID) {
		writeError(w, 404, "PROJECT_NOT_FOUND", "Project 不存在")
		return
	}
	owner := auth.UserIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		if err := h.service.SetProjectRobot(owner, projectID, chi.URLParam(r, "robot_id"), r.Method == http.MethodPut); err != nil {
			writeRobotError(w, err)
			return
		}
	}
	result, err := h.service.ProjectRobots(owner, projectID)
	if err != nil {
		writeRobotError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
