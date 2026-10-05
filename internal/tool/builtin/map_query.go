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

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

const maxMapQueryItems = 100

type mapQueryTool struct{ st *store.Store }

type mapQueryArgs struct {
	MapID       string   `json:"map_id"`
	Generation  int64    `json:"generation"`
	EntityIDs   []string `json:"entity_ids"`
	EntityNames []string `json:"entity_names"`
	EntityTypes []string `json:"entity_types"`
	Status      string   `json:"status"`
	RegionID    string   `json:"region_id"`
	Predicate   string   `json:"predicate"`
	SubjectID   string   `json:"subject_id"`
	ObjectID    string   `json:"object_id"`
	Limit       int      `json:"limit"`
}

func (t *mapQueryTool) Def() tool.Definition {
	return tool.Definition{Name: nameMapQuery, Namespace: "map",
		Description: "查询当前 Project 地图。已知物品/托盘/槽位名称时直接用 entity_names 精确查询，无需猜实体类型或先查 ID。同名会返回多个候选，须消歧，不能直接取第一项。entity_ids 只接受地图返回的内部 id，不能填名称。map_id和generation可省略以使用唯一活动地图；空结果不代表对象在物理场景中不存在，不要切换地图或重复同一查询猜测。",
		ParametersJSON: `{
			"type":"object",
			"properties":{
				"map_id":{"type":"string","enum":["simulation_map","real_map"]},
				"generation":{"type":"integer","minimum":1},
				"entity_ids":{"type":"array","description":"地图内部 id，不是 name；知道名称请用 entity_names","items":{"type":"string"},"maxItems":100},
				"entity_names":{"type":"array","description":"按 name 精确匹配，可批量查询，如 [\"pallet-b-slot-r2-c2\"]；保留所有同名候选","items":{"type":"string"},"maxItems":100},
				"entity_types":{"type":"array","description":"仅填写已知地图类型；知道名称时无需指定类型","items":{"type":"string"},"maxItems":20},
				"status":{"type":"string","enum":["active","uncertain","removed"]},
				"region_id":{"type":"string"},"predicate":{"type":"string"},
				"subject_id":{"type":"string"},"object_id":{"type":"string"},
				"limit":{"type":"integer","minimum":1,"maximum":100}
			},
			"additionalProperties":false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: true}}
}

func (t *mapQueryTool) Run(ctx context.Context, raw string) (string, error) {
	var args mapQueryArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok {
		return "", &tool.Error{Code: "EXECUTION_SCOPE_MISSING", Message: "map.query 只能在已绑定 Project 的 Run 中使用"}
	}
	var err error
	args.MapID = strings.TrimSpace(args.MapID)
	if args.MapID == "" {
		args.MapID, err = t.currentMapSlot(scope.ProjectID)
		if err != nil {
			return "", &tool.Error{Code: "MAP_ID_REQUIRED", Message: err.Error()}
		}
	}
	if !store.ValidMapSlot(args.MapID) {
		return "", &tool.Error{Code: "MAP_ID_INVALID", Message: "map_id 只能是 simulation_map 或 real_map"}
	}
	current, err := t.st.GetSemanticMap(scope.ProjectID, args.MapID)
	if err != nil {
		return "", &tool.Error{Code: "MAP_NOT_FOUND", Message: err.Error()}
	}
	if args.Generation == 0 {
		args.Generation = current.Generation
	} else if args.Generation != current.Generation {
		return "", &tool.Error{Code: "STALE_MAP_GENERATION", Message: store.ErrStaleMapGeneration.Error()}
	}
	if len(args.EntityIDs) == 0 && len(args.EntityNames) == 0 && len(args.EntityTypes) == 0 && args.Status == "" &&
		args.RegionID == "" && args.Predicate == "" && args.SubjectID == "" && args.ObjectID == "" {
		return "", &tool.Error{Code: "MAP_FILTER_REQUIRED", Message: "map.query 必须提供至少一个过滤条件"}
	}
	if args.Limit <= 0 || args.Limit > maxMapQueryItems {
		args.Limit = maxMapQueryItems
	}
	if len(args.EntityIDs) > maxMapQueryItems || len(args.EntityNames) > maxMapQueryItems || len(args.EntityTypes) > 20 {
		return "", &tool.Error{Code: "MAP_FILTER_TOO_LARGE", Message: "entity_ids 最多 100 项，entity_types 最多 20 项"}
	}
	args.EntityIDs = cleanMapQueryStrings(args.EntityIDs)
	args.EntityNames = cleanMapQueryStrings(args.EntityNames)
	args.EntityTypes = cleanMapQueryStrings(args.EntityTypes)
	if len(args.EntityIDs) == 0 && len(args.EntityNames) == 0 && len(args.EntityTypes) == 0 && args.Status == "" &&
		args.RegionID == "" && args.Predicate == "" && args.SubjectID == "" && args.ObjectID == "" {
		return "", &tool.Error{Code: "MAP_FILTER_REQUIRED", Message: "map.query 必须提供至少一个有效过滤条件"}
	}
	snapshot, err := t.st.QuerySemanticMap(scope.ProjectID, args.MapID, store.MapQuery{
		Generation: args.Generation, EntityIDs: args.EntityIDs, EntityNames: args.EntityNames, EntityTypes: args.EntityTypes,
		Status: args.Status, RegionID: args.RegionID, Predicate: args.Predicate,
		SubjectID: args.SubjectID, ObjectID: args.ObjectID, Limit: args.Limit,
	})
	if err != nil {
		return "", &tool.Error{Code: "MAP_QUERY_FAILED", Message: err.Error()}
	}
	return tool.OKResult(map[string]any{"map_id": snapshot.Map.MapID,
		"generation": snapshot.Map.Generation, "revision": snapshot.Map.Revision,
		"frame_id": snapshot.Map.FrameID, "entities": snapshot.Entities,
		"relations": snapshot.Relations, "name_matches": snapshot.NameMatches})
}

func (t *mapQueryTool) currentMapSlot(projectID string) (string, error) {
	simulation, simulationErr := t.st.GetSemanticMap(projectID, store.MapSlotSimulation)
	realMap, realErr := t.st.GetSemanticMap(projectID, store.MapSlotReal)
	if simulationErr != nil && realErr != nil {
		return "", store.ErrNotFound
	}
	if simulationErr != nil {
		return store.MapSlotReal, nil
	}
	if realErr != nil {
		return store.MapSlotSimulation, nil
	}
	// 新Project固定创建两个槽位，但空槽位不构成事实来源。只有一侧真正产生
	// revision时可以安全默认；两侧都有数据时没有“最新即正确”的通用规则。
	if simulation.Revision > 0 && realMap.Revision == 0 {
		return store.MapSlotSimulation, nil
	}
	if realMap.Revision > 0 && simulation.Revision == 0 {
		return store.MapSlotReal, nil
	}
	return "", fmt.Errorf("Project 同时存在 simulation_map 和 real_map，请明确指定 map_id")
}

func cleanMapQueryStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
