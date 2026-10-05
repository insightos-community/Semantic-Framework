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

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/config"
)

// TestMigrateRejectsExperimentalSchema 验证实验版本曾占用的迁移编号不会被
// v0.2.0 误认。拒绝发生在执行新迁移前，数据库内容保持不变。
func TestMigrateRejectsExperimentalSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "experimental.db")
	st, err := Open(config.StoreConfig{Driver: driverSQLite, SQLitePath: path}, testLogger())
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("创建实验迁移表失败: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO schema_migrations (version, name)
		VALUES (10, 'create_task_plans_events_leases')`); err != nil {
		t.Fatalf("写入实验迁移失败: %v", err)
	}
	if err := st.Migrate(); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("实验 schema 应返回 STORE_SCHEMA_INCOMPATIBLE，实际: %v", err)
	}
}

// TestProjectFoundation 验证 Default Project、工作区、会话归属和绑定替换。
func TestProjectFoundation(t *testing.T) {
	st := openTestStore(t)
	project, err := st.EnsureDefaultProject("usr-project")
	if err != nil {
		t.Fatalf("EnsureDefaultProject 失败: %v", err)
	}
	if project.ID != DefaultProjectID("usr-project") || !project.IsDefault {
		t.Fatalf("Default Project 字段不符: %+v", project)
	}
	info, err := os.Stat(project.WorkspaceRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("工作区应以 0700 创建: info=%v err=%v", info, err)
	}

	now := time.Now().UTC()
	session := ChatSession{ID: "cs-project", UserID: "usr-project", Title: "项目会话",
		CreatedAt: now, UpdatedAt: now}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatalf("CreateChatSession 失败: %v", err)
	}
	got, err := st.GetChatSession(session.ID)
	if err != nil || got.ProjectID != project.ID {
		t.Fatalf("会话应归入 Default Project: session=%+v err=%v", got, err)
	}

	bindings := ProjectBindings{AgentIDs: []string{"query-1", "query-1", "leader"},
		SkillNames: []string{"data-profile", "data-profile"}}
	if err := st.ReplaceProjectBindings(context.Background(), project.ID, bindings); err != nil {
		t.Fatalf("ReplaceProjectBindings 失败: %v", err)
	}
	gotBindings, err := st.GetProjectBindings(project.ID)
	if err != nil {
		t.Fatalf("GetProjectBindings 失败: %v", err)
	}
	if len(gotBindings.AgentIDs) != 2 || gotBindings.AgentIDs[0] != "leader" ||
		len(gotBindings.SkillNames) != 1 {
		t.Fatalf("绑定应去重并排序: %+v", gotBindings)
	}
}

func TestProjectRuntimePreferenceAndPortableSceneReference(t *testing.T) {
	st := openTestStore(t)
	project, err := st.CreateProjectWithRuntimePreference(
		"usr-runtime", "Runtime Profile Project", "native-mujoco", "native-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	if project.RuntimeProfileID != "native-mujoco" ||
		project.PreferredRuntimeInstallationID != "native-a" {
		t.Fatalf("Project 未保存 Profile 和本机偏好: %+v", project)
	}

	updated, err := st.SetProjectRuntimePreference(
		"usr-runtime", project.ID, "native-mujoco", "native-b",
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PreferredRuntimeInstallationID != "native-b" ||
		updated.Revision != project.Revision+1 {
		t.Fatalf("启动偏好应允许随当前 Server 安装变化: %+v", updated)
	}
	count, err := st.CountProjectsUsingRuntime("native-b")
	if err != nil || count != 1 {
		t.Fatalf("Runtime 使用计数应读取当前偏好: count=%d err=%v", count, err)
	}

	reference, err := st.AddProjectSceneReference(ProjectSceneReference{
		ProjectID: project.ID, CatalogSceneID: "depalletizing-r1pro",
		SceneVersion: "0.5.0", DefaultVariantID: "layout001",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProjectSceneReference(project.ID, reference.ProjectSceneID)
	if err != nil || got.CatalogSceneID != reference.CatalogSceneID ||
		got.DefaultVariantID != "layout001" {
		t.Fatalf("可移植场景引用 round-trip 失败: got=%+v err=%v", got, err)
	}
	listed, err := st.ListProjectSceneReferences(project.ID)
	if err != nil || len(listed) != 1 || listed[0].ProjectSceneID != reference.ProjectSceneID {
		t.Fatalf("Project 场景资源列表不正确: listed=%+v err=%v", listed, err)
	}
}
