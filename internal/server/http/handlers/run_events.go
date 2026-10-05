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

	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

// HandleRunEvents serves GET /api/v1/runs/{id}/events using the same ownership
// check as Run details. It exposes existing envelopes, not model request dumps
// or a second telemetry stream. Reading history never changes Project presence.
func (h *ProjectsHandler) HandleRunEvents(w http.ResponseWriter, r *http.Request) {
	run, err := h.ownedRun(r)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "Run 不存在")
		return
	}
	if err != nil {
		h.internalError(w, "查询 Run 失败", err)
		return
	}
	after, limit := int64(0), 100
	if raw := r.URL.Query().Get("after_sequence"); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "after_sequence 必须为非负整数")
			return
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 500 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "limit 必须为 1 到 500 的整数")
			return
		}
	}
	records, err := h.st.ListRunEventsAfter(run.ProjectID, run.ID, after, limit+1)
	if err != nil {
		h.internalError(w, "查询 Run 事件失败", err)
		return
	}
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	views := make([]ws.Envelope, 0, len(records))
	next := after
	for _, record := range records {
		var parent ws.ParentRef
		if err := json.Unmarshal([]byte(record.Parent), &parent); err != nil || !json.Valid([]byte(record.Payload)) {
			h.internalError(w, "Run 事件数据损坏", errors.New("invalid persisted event JSON"))
			return
		}
		views = append(views, ws.Envelope{
			ID: record.ID, ProjectID: record.ProjectID, SessionID: record.SessionID,
			ResourceType: record.ResourceType, ResourceID: record.ResourceID,
			Revision: record.Revision, Sequence: record.Sequence, Ts: record.Ts,
			Agent:   ws.AgentRef{ID: record.AgentID, Role: record.AgentRole, Name: record.AgentID},
			Channel: ws.Channel(record.Channel), Type: record.Type,
			Importance: ws.Importance(record.Importance), Parent: parent,
			Payload: json.RawMessage(record.Payload),
		})
		next = record.Sequence
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": views, "has_more": hasMore, "next_sequence": next,
	})
}
