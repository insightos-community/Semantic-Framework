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
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

type mapGenerationRequest struct {
	Revision         int64  `json:"revision"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

type mapQueryRequest struct {
	Generation int64    `json:"generation"`
	EntityID   string   `json:"entity_id"`
	EntityIDs  []string `json:"entity_ids"`
	EntityType string   `json:"entity_type"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	RegionID   string   `json:"region_id"`
	Predicate  string   `json:"predicate"`
	SubjectID  string   `json:"subject_id"`
	ObjectID   string   `json:"object_id"`
}

type mapOperation struct {
	Op         string          `json:"op"`
	Entity     json.RawMessage `json:"entity"`
	EntityID   string          `json:"entity_id"`
	Relation   json.RawMessage `json:"relation"`
	RelationID string          `json:"relation_id"`
}

type mapUpdateRequest struct {
	Generation        int64               `json:"generation"`
	Revision          int64               `json:"revision"`
	ExpectedRevision  int64               `json:"expected_revision"`
	Source            string              `json:"source"`
	Operations        []mapOperation      `json:"operations"`
	Entities          []store.MapEntity   `json:"entities"`
	RemoveEntityIDs   []string            `json:"remove_entity_ids"`
	Relations         []store.MapRelation `json:"relations"`
	RemoveRelationIDs []string            `json:"remove_relation_ids"`
}

type mapEntityInput struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Name            string          `json:"name"`
	Status          string          `json:"status"`
	FrameID         string          `json:"frame_id"`
	Pose            store.Pose      `json:"pose"`
	Bounds          store.Bounds    `json:"bounds"`
	Geometry        store.Bounds    `json:"geometry"`
	Properties      map[string]any  `json:"properties"`
	SourceTimestamp *time.Time      `json:"source_timestamp"`
	Evidence        []string        `json:"evidence"`
	ManualFields    map[string]bool `json:"manual_fields"`
}

func (v mapEntityInput) storeValue() store.MapEntity {
	bounds := v.Bounds
	if bounds.Kind == "" {
		bounds = v.Geometry
	}
	pose := v.Pose
	if pose.Orientation == (store.Quaternion{}) {
		pose.Orientation.W = 1
	}
	return store.MapEntity{ID: v.ID, Type: v.Type, Name: v.Name, Status: v.Status,
		FrameID: v.FrameID, Pose: pose, Bounds: bounds, Properties: v.Properties,
		SourceTimestamp: v.SourceTimestamp, Evidence: v.Evidence, ManualFields: v.ManualFields}
}

// HandleGetSemanticMap 返回指定 slot 的当前或历史 generation。
func (h *ProjectsHandler) HandleGetSemanticMap(w http.ResponseWriter, r *http.Request) {
	project, slot, ok := h.ownedMap(w, r)
	if !ok {
		return
	}
	var generation int64
	if rawGeneration := r.URL.Query().Get("generation"); rawGeneration != "" {
		parsed, err := strconv.ParseInt(rawGeneration, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_GENERATION", "generation 必须是非负整数")
			return
		}
		generation = parsed
	}
	snapshot, err := h.st.GetMapSnapshot(project.ID, slot, generation)
	if h.writeV030Error(w, err, "读取 Semantic Map 失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"map_snapshot": publicMapSnapshot(snapshot)})
}

// HandleQuerySemanticMap 根据 ID、类型、区域和稳定关系读取地图子集。
func (h *ProjectsHandler) HandleQuerySemanticMap(w http.ResponseWriter, r *http.Request) {
	project, slot, ok := h.ownedMap(w, r)
	if !ok {
		return
	}
	var request mapQueryRequest
	if !decodeV030JSON(w, r, &request) {
		return
	}
	if request.Generation < 0 {
		writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_QUERY", "generation 不能小于 0")
		return
	}
	entityType := request.EntityType
	if entityType == "" {
		entityType = request.Type
	}
	query := store.MapQuery{
		Generation: request.Generation, EntityID: request.EntityID, EntityType: entityType,
		Status: request.Status, RegionID: request.RegionID, Predicate: request.Predicate,
		SubjectID: request.SubjectID, ObjectID: request.ObjectID,
	}
	if len(request.EntityIDs) > 0 {
		if request.EntityID != "" || entityType != "" || request.Status != "" || request.RegionID != "" ||
			request.Predicate != "" || request.SubjectID != "" || request.ObjectID != "" {
			writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_QUERY",
				"entity_ids 不能与其他地图筛选条件同时使用")
			return
		}
		allowed := make(map[string]struct{}, len(request.EntityIDs))
		for _, id := range request.EntityIDs {
			if strings.TrimSpace(id) == "" {
				writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_QUERY", "entity_ids 不能为空")
				return
			}
			allowed[id] = struct{}{}
		}
		var snapshot store.MapSnapshot
		seenEntities := make(map[string]struct{}, len(allowed))
		seenRelations := make(map[string]struct{})
		for _, id := range request.EntityIDs {
			part, err := h.st.QuerySemanticMap(project.ID, slot, store.MapQuery{
				Generation: request.Generation, EntityID: id,
			})
			if h.writeV030Error(w, err, "查询 Semantic Map 失败") {
				return
			}
			if snapshot.Map.Slot == "" {
				snapshot.Map, snapshot.Generation, snapshot.ReadOnly = part.Map, part.Generation, part.ReadOnly
			}
			for _, entity := range part.Entities {
				if _, selected := allowed[entity.ID]; !selected {
					continue
				}
				if _, exists := seenEntities[entity.ID]; !exists {
					snapshot.Entities = append(snapshot.Entities, entity)
					seenEntities[entity.ID] = struct{}{}
				}
			}
			for _, relation := range part.Relations {
				if _, exists := seenRelations[relation.ID]; !exists {
					snapshot.Relations = append(snapshot.Relations, relation)
					seenRelations[relation.ID] = struct{}{}
				}
			}
		}
		snapshot.Relations = relationsWithinEntities(snapshot.Relations, snapshot.Entities)
		writeJSON(w, http.StatusOK, map[string]any{"map_view": publicMapSnapshot(snapshot)})
		return
	}
	snapshot, err := h.st.QuerySemanticMap(project.ID, slot, query)
	if h.writeV030Error(w, err, "查询 Semantic Map 失败") {
		return
	}
	snapshot.Relations = relationsWithinEntities(snapshot.Relations, snapshot.Entities)
	writeJSON(w, http.StatusOK, map[string]any{"map_view": publicMapSnapshot(snapshot)})
}

func relationsWithinEntities(relations []store.MapRelation, entities []store.MapEntity) []store.MapRelation {
	visible := make(map[string]struct{}, len(entities))
	for _, entity := range entities {
		visible[entity.ID] = struct{}{}
	}
	filtered := make([]store.MapRelation, 0, len(relations))
	for _, relation := range relations {
		_, subjectVisible := visible[relation.SubjectID]
		_, objectVisible := visible[relation.ObjectID]
		if subjectVisible && objectVisible {
			filtered = append(filtered, relation)
		}
	}
	return filtered
}

// HandleCreateMapGeneration 关闭当前 generation 并创建空的新 generation。
func (h *ProjectsHandler) HandleCreateMapGeneration(w http.ResponseWriter, r *http.Request) {
	project, slot, ok := h.ownedMap(w, r)
	if !ok {
		return
	}
	if !h.writableProject(w, r, project.ID) {
		return
	}
	var request mapGenerationRequest
	if !decodeV030JSON(w, r, &request) {
		return
	}
	revision := request.ExpectedRevision
	if request.ExpectedRevision == 0 {
		revision = request.Revision
	}
	if revision < 0 {
		writeError(w, http.StatusUnprocessableEntity, "INVALID_REVISION", "revision 不能小于 0")
		return
	}
	updated, err := h.st.CreateMapGeneration(project.ID, slot, revision,
		strings.TrimSpace(request.Reason), time.Now().UTC())
	if h.writeV030Error(w, err, "创建地图 generation 失败") {
		return
	}
	snapshot, err := h.st.GetMapSnapshot(project.ID, slot, updated.Generation)
	if h.writeV030Error(w, err, "读取新地图 generation 失败") {
		return
	}
	h.publishMapSnapshot(snapshot, "semantic_map.generation_created", nil)
	writeJSON(w, http.StatusCreated, map[string]any{"map_snapshot": publicMapSnapshot(snapshot)})
}

// HandleUpdateSemanticMap 将一次人工 Entity/Relation 操作作为一个 revision 提交。
func (h *ProjectsHandler) HandleUpdateSemanticMap(w http.ResponseWriter, r *http.Request) {
	project, slot, ok := h.ownedMap(w, r)
	if !ok {
		return
	}
	if !h.writableProject(w, r, project.ID) {
		return
	}
	var request mapUpdateRequest
	if !decodeV030JSON(w, r, &request) {
		return
	}
	if request.Source != "" && request.Source != store.MapSourceUser {
		writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_SOURCE", "公共接口只接受人工地图更新")
		return
	}
	for _, operation := range request.Operations {
		switch operation.Op {
		case "upsert_entity":
			var input mapEntityInput
			if len(operation.Entity) == 0 || json.Unmarshal(operation.Entity, &input) != nil {
				writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_OPERATION", "upsert_entity 缺少合法 entity")
				return
			}
			request.Entities = append(request.Entities, input.storeValue())
		case "remove_entity":
			if strings.TrimSpace(operation.EntityID) == "" {
				writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_OPERATION", "remove_entity 缺少 entity_id")
				return
			}
			request.RemoveEntityIDs = append(request.RemoveEntityIDs, operation.EntityID)
		case "upsert_relation":
			var relation store.MapRelation
			if len(operation.Relation) == 0 || json.Unmarshal(operation.Relation, &relation) != nil {
				writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_OPERATION", "upsert_relation 缺少合法 relation")
				return
			}
			request.Relations = append(request.Relations, relation)
		case "remove_relation":
			if strings.TrimSpace(operation.RelationID) == "" {
				writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_OPERATION", "remove_relation 缺少 relation_id")
				return
			}
			request.RemoveRelationIDs = append(request.RemoveRelationIDs, operation.RelationID)
		default:
			writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_OPERATION", "不支持的地图操作")
			return
		}
	}
	if request.Generation <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "INVALID_MAP_GENERATION", "generation 必须大于 0")
		return
	}
	if len(request.Entities)+len(request.RemoveEntityIDs)+len(request.Relations)+len(request.RemoveRelationIDs) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "EMPTY_MAP_UPDATE", "地图更新不能为空")
		return
	}
	revision := request.ExpectedRevision
	if request.ExpectedRevision == 0 {
		revision = request.Revision
	}
	snapshot, err := h.st.ApplyMapUpdate(project.ID, slot, store.MapUpdate{
		Generation: request.Generation, ExpectedRevision: revision, Source: store.MapSourceUser,
		Entities: request.Entities, RemoveEntityIDs: request.RemoveEntityIDs,
		Relations: request.Relations, RemoveRelationIDs: request.RemoveRelationIDs,
	}, time.Now().UTC())
	if h.writeV030Error(w, err, "更新 Semantic Map 失败") {
		return
	}
	h.publishMapSnapshot(snapshot, "semantic_map.updated", request.Operations)
	writeJSON(w, http.StatusOK, map[string]any{"map_snapshot": publicMapSnapshot(snapshot)})
}

func (h *ProjectsHandler) ownedMap(w http.ResponseWriter, r *http.Request) (store.Project, string, bool) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return store.Project{}, "", false
	}
	slot := chi.URLParam(r, "map_id")
	if !store.ValidMapSlot(slot) {
		writeError(w, http.StatusNotFound, "MAP_NOT_FOUND", "Semantic Map 不存在")
		return store.Project{}, "", false
	}
	return project, slot, true
}

func publicMapSnapshot(snapshot store.MapSnapshot) map[string]any {
	return map[string]any{
		"map_id": snapshot.Map.Slot, "slot": snapshot.Map.Slot,
		"project_id": snapshot.Map.ProjectID, "generation": snapshot.Map.Generation,
		"revision": snapshot.Map.Revision, "frame_id": snapshot.Map.FrameID,
		"created_at": snapshot.Map.CreatedAt, "updated_at": snapshot.Map.UpdatedAt,
		"generation_view": snapshot.Generation, "read_only": snapshot.ReadOnly,
		"entities": snapshot.Entities, "relations": snapshot.Relations,
	}
}

func (h *ProjectsHandler) publishMapSnapshot(snapshot store.MapSnapshot, eventType string, operations []mapOperation) {
	payload := map[string]any{"map_id": snapshot.Map.Slot, "generation": snapshot.Map.Generation,
		"revision": snapshot.Map.Revision, "map_snapshot": publicMapSnapshot(snapshot)}
	h.publishResourceEvent(snapshot.Map.ProjectID, "", "semantic_map", snapshot.Map.Slot,
		snapshot.Map.Revision, eventType, payload, ws.ParentRef{})
	for _, operation := range operations {
		switch operation.Op {
		case "upsert_entity":
			var entity mapEntityInput
			_ = json.Unmarshal(operation.Entity, &entity)
			if entity.ID != "" {
				h.publishResourceEvent(snapshot.Map.ProjectID, "", "map_entity", entity.ID,
					snapshot.Map.Revision, "map_entity.updated", payload, ws.ParentRef{})
			}
		case "remove_entity":
			h.publishResourceEvent(snapshot.Map.ProjectID, "", "map_entity", operation.EntityID,
				snapshot.Map.Revision, "map_entity.removed", payload, ws.ParentRef{})
		case "upsert_relation":
			var relation store.MapRelation
			_ = json.Unmarshal(operation.Relation, &relation)
			if relation.ID != "" {
				h.publishResourceEvent(snapshot.Map.ProjectID, "", "map_relation", relation.ID,
					snapshot.Map.Revision, "map_relation.updated", payload, ws.ParentRef{})
			}
		case "remove_relation":
			h.publishResourceEvent(snapshot.Map.ProjectID, "", "map_relation", operation.RelationID,
				snapshot.Map.Revision, "map_relation.removed", payload, ws.ParentRef{})
		}
	}
}
