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
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

func TestMapQueryProjectIsolationAndGeneration(t *testing.T) {
	registry, st := newTestRegistry(t)
	first, err := st.EnsureDefaultProject("map-user-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.EnsureDefaultProject("map-user-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		project store.Project
		id      string
	}{{first, "entity-a"}, {second, "entity-b"}} {
		_, err = st.ApplyMapUpdate(item.project.ID, store.MapSlotSimulation, store.MapUpdate{
			Generation: 1, ExpectedRevision: 0, Source: store.MapSourceUser,
			Entities: []store.MapEntity{{ID: item.id, Type: "box", Name: item.id,
				Status: store.EntityStatusActive, FrameID: "world",
				Pose: store.Pose{Orientation: store.Quaternion{W: 1}}}},
		}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
	}
	mapTool, _ := registry.Get(nameMapQuery)
	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "session-a", ProjectID: first.ID, OwnerID: first.OwnerID,
		WorkspaceRoot: first.WorkspaceRoot,
	})
	out, err := mapTool.Run(ctx, `{"map_id":"simulation_map","generation":1,"entity_ids":["entity-a"],"limit":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "entity-a") || strings.Contains(out, "entity-b") {
		t.Fatalf("map.query 泄漏其他 Project 或缺少目标: %s", out)
	}
	out, err = mapTool.Run(ctx, `{"entity_ids":["entity-a"]}`)
	if err != nil || !strings.Contains(out, "entity-a") {
		t.Fatalf("只有一个Map来源时应允许省略map_id和generation: out=%s err=%v", out, err)
	}
	if _, err = st.ApplyMapUpdate(first.ID, store.MapSlotReal, store.MapUpdate{
		Generation: 1, ExpectedRevision: 0, Source: store.MapSourceUser,
		Entities: []store.MapEntity{{ID: "real-entity", Type: "box", Name: "real-entity",
			Status: store.EntityStatusActive, FrameID: "world",
			Pose: store.Pose{Orientation: store.Quaternion{W: 1}}}},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err = mapTool.Run(ctx, `{"entity_ids":["entity-a"]}`); err == nil ||
		!strings.Contains(err.Error(), "MAP_ID_REQUIRED") {
		t.Fatalf("多个Map来源时不得按更新时间静默选择: %v", err)
	}
	if _, err := st.CreateMapGeneration(first.ID, store.MapSlotSimulation, 1, "reset", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := mapTool.Run(ctx, `{"map_id":"simulation_map","generation":1,"entity_ids":["entity-a"]}`); err == nil || !strings.Contains(err.Error(), "STALE") {
		t.Fatalf("旧 generation 应被拒绝: %v", err)
	}
}

func TestMapQueryExactNamesPreserveAllCandidates(t *testing.T) {
	registry, st := newTestRegistry(t)
	p, _ := st.EnsureDefaultProject("name-query")
	_, err := st.ApplyMapUpdate(p.ID, store.MapSlotSimulation, store.MapUpdate{Generation: 1, Source: store.MapSourceUser,
		Entities: []store.MapEntity{
			{ID: "id-one", Name: "目标槽位", Type: "region", Status: "active", FrameID: "world", Pose: store.Pose{Orientation: store.Quaternion{W: 1}}},
			{ID: "id-two", Name: "目标槽位", Type: "region", Status: "active", FrameID: "world", Pose: store.Pose{Orientation: store.Quaternion{W: 1}}},
			{ID: "id-three", Name: "目标槽位-extra", Type: "region", Status: "active", FrameID: "world", Pose: store.Pose{Orientation: store.Quaternion{W: 1}}},
		}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	query, _ := registry.Get(nameMapQuery)
	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{ProjectID: p.ID, SessionID: "name-query-session", OwnerID: p.OwnerID, WorkspaceRoot: p.WorkspaceRoot})
	out, err := query.Run(ctx, `{"entity_names":["目标槽位"]}`)
	if err != nil || !strings.Contains(out, "id-one") || !strings.Contains(out, "id-two") || strings.Contains(out, "id-three") {
		t.Fatalf("exact candidates: %s %v", out, err)
	}
	out, err = query.Run(ctx, `{"entity_names":["目标槽位"],"limit":1}`)
	if err != nil || !strings.Contains(out, `"目标槽位":2`) {
		t.Fatalf("limit must not hide ambiguity: %s %v", out, err)
	}
	out, err = query.Run(ctx, `{"entity_ids":["目标槽位"]}`)
	if err != nil || strings.Contains(out, "id-one") {
		t.Fatalf("IDs must remain unambiguous: %s %v", out, err)
	}
	if _, err = query.Run(ctx, `{"entity_names":["  "]}`); err == nil {
		t.Fatal("empty name filter accepted")
	}
	out, err = query.Run(ctx, `{"entity_names":["' OR 1=1 --"]}`)
	if err != nil || strings.Contains(out, "id-one") {
		t.Fatal("name must be a parameterized literal")
	}
}

func TestMapQueryLimitKeepsRelationEndpoints(t *testing.T) {
	registry, st := newTestRegistry(t)
	project, err := st.EnsureDefaultProject("map-limit-user")
	if err != nil {
		t.Fatal(err)
	}
	entities := []store.MapEntity{
		{ID: "a", Type: "box", Name: "a", Status: store.EntityStatusActive, FrameID: "world", Pose: store.Pose{Orientation: store.Quaternion{W: 1}}},
		{ID: "b", Type: "box", Name: "b", Status: store.EntityStatusActive, FrameID: "world", Pose: store.Pose{Orientation: store.Quaternion{W: 1}}},
		{ID: "c", Type: "box", Name: "c", Status: store.EntityStatusActive, FrameID: "world", Pose: store.Pose{Orientation: store.Quaternion{W: 1}}},
	}
	_, err = st.ApplyMapUpdate(project.ID, store.MapSlotSimulation, store.MapUpdate{
		Generation: 1, ExpectedRevision: 0, Source: store.MapSourceUser, Entities: entities,
		Relations: []store.MapRelation{
			{ID: "rel-ab", SubjectID: "a", Predicate: "contains", ObjectID: "b"},
			{ID: "rel-ac", SubjectID: "a", Predicate: "supports", ObjectID: "c"},
		},
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	mapTool, _ := registry.Get(nameMapQuery)
	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "s", ProjectID: project.ID, OwnerID: project.OwnerID,
		WorkspaceRoot: project.WorkspaceRoot,
	})
	out, err := mapTool.Run(ctx, `{"map_id":"simulation_map","generation":1,"entity_ids":["a"],"limit":1}`)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			Entities  []store.MapEntity   `json:"entities"`
			Relations []store.MapRelation `json:"relations"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, entity := range envelope.Data.Entities {
		ids[entity.ID] = true
	}
	for _, relation := range envelope.Data.Relations {
		if !ids[relation.SubjectID] || !ids[relation.ObjectID] {
			t.Fatalf("limit 产生悬空关系: %+v entities=%v", relation, ids)
		}
	}
}
