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
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// SystemDefaultProjectID 是无用户归属的系统运行使用的 Default Project。
	SystemDefaultProjectID = "proj-default-system"

	// ProjectModeDevelopment 表示 Project 可编辑、可对话。
	ProjectModeDevelopment = "development"

	// ProjectModeRunning 为后续 Workflow 执行保留。v0.2 的公共接口不开放
	// 手工切换到该模式，避免绕过后续的计划确认流程。
	ProjectModeRunning = "running"
)

const projectSelectColumns = `id, owner_id, name, workspace_root, is_default,
	mode, is_active, runtime_profile_id, preferred_runtime_installation_id,
	archived_at, revision, created_at, updated_at`

// Project 是会话、Agent 与 Skill 的工作区容器。IsActive 是整个安装环境
// 的唯一活动 Project；Mode 只表达开发或运行方式，不承担用户权限。
type Project struct {
	ID            string `json:"id"`
	OwnerID       string `json:"owner_id,omitempty"`
	Name          string `json:"name"`
	WorkspaceRoot string `json:"workspace_root"`
	IsDefault     bool   `json:"is_default"`
	Mode          string `json:"mode"`
	IsActive      bool   `json:"is_active"`
	// RuntimeProfileID 是 Project 可移植的运行能力偏好；Installation 只是当前
	// Server 上的启动偏好，可以在实际启动时重新选择，不能写进 Scene 引用。
	RuntimeProfileID               string `json:"runtime_profile_id,omitempty"`
	PreferredRuntimeInstallationID string `json:"preferred_runtime_installation_id,omitempty"`

	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	Revision   int64      `json:"revision"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ProjectBindings 保存 Project 明确绑定的 Agent 实例和 Skill 名称。空列表
// 表示不增加 Project 级限制，便于从旧数据迁移到 Default Project。
type ProjectBindings struct {
	AgentIDs   []string `json:"agent_ids"`
	SkillNames []string `json:"skill_names"`
}

// DefaultProjectID 返回用户稳定的 Default Project ID。
func DefaultProjectID(ownerID string) string {
	if ownerID == "" {
		return SystemDefaultProjectID
	}
	return "proj-default-" + ownerID
}

// NewProjectID 生成普通 Project ID。
func NewProjectID() string { return "proj-" + uuid.NewString() }

// EnsureDefaultProject 幂等创建用户 Default Project，并确保工作区目录存在。
// 非系统用户第一次获得 Project 时自动激活；系统 Default Project 不占用
// Studio 的唯一活动位置。
func (s *Store) EnsureDefaultProject(ownerID string) (Project, error) {
	id := DefaultProjectID(ownerID)
	root := filepath.Join(filepath.Dir(s.databasePath), "workspaces", id)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Project{}, fmt.Errorf("创建 Project 工作区失败: %w", err)
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return Project{}, fmt.Errorf("开启 Default Project 事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO projects
		(id, owner_id, name, workspace_root, is_default, mode, is_active,
		 revision, created_at, updated_at)
		VALUES (?, ?, 'Default Project', ?, 1, ?, 0, 1, ?, ?)`,
		id, ownerID, root, ProjectModeDevelopment, now, now); err != nil {
		return Project{}, fmt.Errorf("初始化 Default Project 失败: %w", err)
	}
	if ownerID != "" {
		if err := activateIfNoneTx(tx, id, now); err != nil {
			return Project{}, err
		}
	}
	if err := ensureSemanticMapsTx(tx, id, now); err != nil {
		return Project{}, fmt.Errorf("初始化 Default Project 地图失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("提交 Default Project 事务失败: %w", err)
	}
	project, err := s.GetProject(id)
	if err != nil {
		return Project{}, err
	}
	return project, nil
}

// activateIfNoneTx 只在安装环境当前没有活动 Project 时激活目标。
func activateIfNoneTx(tx *sql.Tx, projectID string, now time.Time) error {
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM projects
		WHERE is_active = 1 AND archived_at IS NULL`).Scan(&active); err != nil {
		return fmt.Errorf("检查活动 Project 失败: %w", err)
	}
	if active != 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE projects SET is_active = 1,
		revision = revision + 1, updated_at = ?
		WHERE id = ? AND archived_at IS NULL`, now, projectID); err != nil {
		return fmt.Errorf("激活首个 Project 失败: %w", err)
	}
	return nil
}

// ensureMigratedDefaultProjects 把 v1-v9 的用户和会话归入 Default Project。
func (s *Store) ensureMigratedDefaultProjects() error {
	if _, err := s.EnsureDefaultProject(""); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT owner_id FROM (
		SELECT id AS owner_id FROM users
		UNION
		SELECT DISTINCT user_id AS owner_id FROM chat_sessions
	) WHERE owner_id <> '' ORDER BY owner_id`)
	if err != nil {
		return fmt.Errorf("查询 Default Project 属主失败: %w", err)
	}
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			_ = rows.Close()
			return fmt.Errorf("扫描 Default Project 属主失败: %w", err)
		}
		owners = append(owners, owner)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭 Default Project 属主查询失败: %w", err)
	}
	for _, owner := range owners {
		project, err := s.EnsureDefaultProject(owner)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE chat_sessions SET project_id = ?
			WHERE user_id = ? AND project_id = ''`, project.ID, owner); err != nil {
			return fmt.Errorf("迁移用户 %q 的会话 Project 归属失败: %w", owner, err)
		}
	}
	return nil
}

// CreateProject 创建普通 Project，工作区固定分配到数据库同级目录，调用方
// 不能提交任意宿主绝对路径。整个环境尚无活动 Project 时自动激活本项目。
func (s *Store) CreateProject(ownerID, name string) (Project, error) {
	return s.CreateProjectWithRuntimePreference(ownerID, name, "", "")
}

// CreateProjectWithRuntimePreference 保存可移植的 Profile 与本机 Installation
// 偏好。真正启动 Scene 时仍会按当前安装清单重新选择并验证兼容性。
func (s *Store) CreateProjectWithRuntimePreference(
	ownerID, name, profileID, preferredInstallationID string,
) (Project, error) {
	name = strings.TrimSpace(name)
	profileID = strings.TrimSpace(profileID)
	preferredInstallationID = strings.TrimSpace(preferredInstallationID)
	if ownerID == "" || name == "" {
		return Project{}, fmt.Errorf("Project owner_id 和 name 均不能为空")
	}
	id := NewProjectID()
	root := filepath.Join(filepath.Dir(s.databasePath), "workspaces", id)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Project{}, fmt.Errorf("创建 Project 工作区失败: %w", err)
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return Project{}, fmt.Errorf("开启 Project 创建事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO projects
		(id, owner_id, name, workspace_root, is_default, mode, is_active,
		 runtime_profile_id, preferred_runtime_installation_id,
		 revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, 0, ?, ?, 1, ?, ?)`,
		id, ownerID, name, root, ProjectModeDevelopment, profileID,
		preferredInstallationID, now, now); err != nil {
		return Project{}, fmt.Errorf("创建 Project %q 失败: %w", id, err)
	}
	if err := activateIfNoneTx(tx, id, now); err != nil {
		return Project{}, err
	}
	if err := ensureSemanticMapsTx(tx, id, now); err != nil {
		return Project{}, fmt.Errorf("初始化 Project 地图失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("提交 Project 创建事务失败: %w", err)
	}
	project, err := s.GetProject(id)
	if err != nil {
		return Project{}, err
	}
	return project, nil
}

// projectScanner 是 sql.Row 与 sql.Rows 共同满足的最小扫描接口。
type projectScanner interface {
	Scan(dest ...any) error
}

func scanProject(scanner projectScanner) (Project, error) {
	var project Project
	var isDefault, isActive int
	if err := scanner.Scan(&project.ID, &project.OwnerID, &project.Name,
		&project.WorkspaceRoot, &isDefault, &project.Mode, &isActive,
		&project.RuntimeProfileID, &project.PreferredRuntimeInstallationID,
		&project.ArchivedAt, &project.Revision, &project.CreatedAt,
		&project.UpdatedAt); err != nil {
		return Project{}, err
	}
	project.IsDefault = isDefault != 0
	project.IsActive = isActive != 0
	return project, nil
}

// GetProject 按 ID 查询 Project。
func (s *Store) GetProject(id string) (Project, error) {
	project, err := scanProject(s.db.QueryRow(`SELECT `+projectSelectColumns+
		` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("查询 Project %q 失败: %w", id, err)
	}
	return project, nil
}

// GetActiveProject 返回当前用户拥有的活动 Project。
func (s *Store) GetActiveProject(ownerID string) (Project, error) {
	project, err := scanProject(s.db.QueryRow(`SELECT `+projectSelectColumns+
		` FROM projects WHERE owner_id = ? AND is_active = 1 AND archived_at IS NULL`,
		ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("查询活动 Project 失败: %w", err)
	}
	return project, nil
}

// ListDevelopmentProjects 仅供 Server 扫描项目投递目录；HTTP 仍按用户校验所有权。
func (s *Store) ListDevelopmentProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectSelectColumns + ` FROM projects WHERE archived_at IS NULL AND mode = 'development' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

// ListProjects 返回当前用户的 Project，默认不返回已归档项目。
func (s *Store) ListProjects(ownerID string, includeArchived bool) ([]Project, error) {
	query := `SELECT ` + projectSelectColumns + ` FROM projects WHERE owner_id = ?`
	if !includeArchived {
		query += ` AND archived_at IS NULL`
	}
	query += ` ORDER BY is_active DESC, updated_at DESC, id`
	rows, err := s.db.Query(query, ownerID)
	if err != nil {
		return nil, fmt.Errorf("查询 Project 列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	projects := make([]Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描 Project 失败: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 Project 列表失败: %w", err)
	}
	return projects, nil
}

// UpdateProjectName 使用 revision 条件更新活动 Project 的名称。
func (s *Store) UpdateProjectName(ownerID, projectID, name string, expectedRevision int64) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" || expectedRevision <= 0 {
		return Project{}, fmt.Errorf("Project name 和 revision 必须有效")
	}
	now := time.Now().UTC()
	result, err := s.db.Exec(`UPDATE projects SET name = ?, revision = revision + 1,
		updated_at = ? WHERE id = ? AND owner_id = ? AND is_active = 1
		AND archived_at IS NULL AND revision = ?`,
		name, now, projectID, ownerID, expectedRevision)
	if err != nil {
		return Project{}, fmt.Errorf("更新 Project 失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Project{}, s.projectWriteError(ownerID, projectID, expectedRevision)
	}
	return s.GetProject(projectID)
}

func (s *Store) projectWriteError(ownerID, projectID string, expectedRevision int64) error {
	project, err := s.GetProject(projectID)
	if err != nil || project.OwnerID != ownerID {
		return ErrNotFound
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	if !project.IsActive {
		return ErrProjectInactive
	}
	if project.Revision != expectedRevision {
		return ErrRevisionConflict
	}
	return ErrInvalidState
}

// ActivateProject 切换安装环境唯一的活动 Project。切换和两个 Project 的
// revision 更新在同一事务中完成，Studio 不会观察到两个同时活动的状态。
func (s *Store) ActivateProject(ownerID, projectID string) (Project, error) {
	s.projectWriteMu.Lock()
	defer s.projectWriteMu.Unlock()
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return Project{}, fmt.Errorf("开启 Project 激活事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	project, err := scanProject(tx.QueryRow(`SELECT `+projectSelectColumns+
		` FROM projects WHERE id = ? AND owner_id = ?`, projectID, ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("查询待激活 Project 失败: %w", err)
	}
	if project.ArchivedAt != nil {
		return Project{}, ErrProjectArchived
	}
	if !project.IsActive {
		var hasActiveWork int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM projects p WHERE p.is_active=1 AND p.id<>? AND (EXISTS(SELECT 1 FROM workflows w WHERE w.project_id=p.id AND w.status IN ('pending','running','paused','stopping')) OR EXISTS(SELECT 1 FROM run_sessions r WHERE r.project_id=p.id AND r.status IN ('queued','running','waiting_input','cancelling')) OR EXISTS(SELECT 1 FROM interactions i WHERE i.project_id=p.id AND i.status='pending')))`, projectID).Scan(&hasActiveWork); err != nil {
			return Project{}, fmt.Errorf("检查当前 Project 未结束工作失败: %w", err)
		}
		if hasActiveWork != 0 {
			return Project{}, ErrProjectHasActiveWork
		}
		if _, err := tx.Exec(`UPDATE projects SET is_active = 0,
			revision = revision + 1, updated_at = ?
			WHERE is_active = 1`, now); err != nil {
			return Project{}, fmt.Errorf("停用原活动 Project 失败: %w", err)
		}
		if _, err := tx.Exec(`UPDATE projects SET is_active = 1,
			revision = revision + 1, updated_at = ? WHERE id = ?`,
			now, projectID); err != nil {
			return Project{}, fmt.Errorf("激活 Project 失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("提交 Project 激活事务失败: %w", err)
	}
	return s.GetProject(projectID)
}

// ArchiveProject 归档普通 Project。若它当前活动，则自动回到同一用户最近的
// 未归档 Project（优先 Default Project），保证 Studio 始终有可用工作区。
func (s *Store) ArchiveProject(ownerID, projectID string, expectedRevision int64) (Project, error) {
	if expectedRevision <= 0 {
		return Project{}, fmt.Errorf("revision 必须大于 0")
	}
	s.projectWriteMu.Lock()
	defer s.projectWriteMu.Unlock()
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return Project{}, fmt.Errorf("开启 Project 归档事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	project, err := scanProject(tx.QueryRow(`SELECT `+projectSelectColumns+
		` FROM projects WHERE id = ? AND owner_id = ?`, projectID, ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("查询待归档 Project 失败: %w", err)
	}
	if project.IsDefault {
		return Project{}, ErrDefaultProject
	}
	if project.ArchivedAt != nil {
		return project, nil
	}
	if project.Revision != expectedRevision {
		return Project{}, ErrRevisionConflict
	}

	// 检查与归档更新必须处于同一事务。否则另一个写入口可能在检查之后
	// 创建 Run/Interaction，导致 Project 已隐藏但执行仍在继续。所有生产
	// Conversation 写入口还会经过 projectWriteMu，关闭事务提交后的反向竞态。
	var hasActiveRun, hasPendingInteraction, hasActiveWorkflow int
	if err := tx.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM run_sessions WHERE project_id = ? AND status IN (?, ?, ?, ?)),
		EXISTS(SELECT 1 FROM interactions WHERE project_id = ? AND status = ?),
		EXISTS(SELECT 1 FROM workflows WHERE project_id = ? AND status IN ('pending','running','paused','stopping'))`,
		projectID, RunStatusQueued, RunStatusRunning, RunStatusWaitingInput,
		RunStatusCancelling, projectID, InteractionStatusPending, projectID).Scan(
		&hasActiveRun, &hasPendingInteraction, &hasActiveWorkflow); err != nil {
		return Project{}, fmt.Errorf("检查 Project 未结束工作失败: %w", err)
	}
	if hasActiveRun != 0 || hasPendingInteraction != 0 || hasActiveWorkflow != 0 {
		return Project{}, ErrProjectHasActiveWork
	}

	var replacementID string
	if project.IsActive {
		if err := tx.QueryRow(`SELECT id FROM projects
			WHERE owner_id = ? AND id <> ? AND archived_at IS NULL
			ORDER BY is_default DESC, updated_at DESC LIMIT 1`,
			ownerID, projectID).Scan(&replacementID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Project{}, fmt.Errorf("没有可切换的 Project: %w", ErrInvalidState)
			}
			return Project{}, fmt.Errorf("选择替代 Project 失败: %w", err)
		}
	}
	result, err := tx.Exec(`UPDATE projects SET archived_at = ?, is_active = 0,
		revision = revision + 1, updated_at = ?
		WHERE id = ? AND owner_id = ? AND revision = ?`,
		now, now, projectID, ownerID, expectedRevision)
	if err != nil {
		return Project{}, fmt.Errorf("归档 Project 失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Project{}, ErrRevisionConflict
	}
	if replacementID != "" {
		if _, err := tx.Exec(`UPDATE projects SET is_active = 1,
			revision = revision + 1, updated_at = ? WHERE id = ?`,
			now, replacementID); err != nil {
			return Project{}, fmt.Errorf("激活替代 Project 失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("提交 Project 归档事务失败: %w", err)
	}
	return s.GetProject(projectID)
}

// SetProjectMode 供后续已经完成计划确认的运行服务切换模式。v0.2 HTTP 不直接
// 暴露该操作；条件更新防止旧状态覆盖 Studio 刚完成的修改。
func (s *Store) SetProjectMode(projectID, mode string, expectedRevision int64) (Project, error) {
	if mode != ProjectModeDevelopment && mode != ProjectModeRunning {
		return Project{}, ErrInvalidState
	}
	result, err := s.db.Exec(`UPDATE projects SET mode = ?, revision = revision + 1,
		updated_at = ? WHERE id = ? AND is_active = 1 AND archived_at IS NULL
		AND revision = ?`, mode, time.Now().UTC(), projectID, expectedRevision)
	if err != nil {
		return Project{}, fmt.Errorf("更新 Project 模式失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		project, getErr := s.GetProject(projectID)
		if getErr != nil {
			return Project{}, getErr
		}
		if project.ArchivedAt != nil {
			return Project{}, ErrProjectArchived
		}
		if !project.IsActive {
			return Project{}, ErrProjectInactive
		}
		return Project{}, ErrRevisionConflict
	}
	return s.GetProject(projectID)
}

// ReplaceProjectBindings 原子替换 Project 的 Agent 与 Skill 绑定。
func (s *Store) ReplaceProjectBindings(ctx context.Context, projectID string, bindings ProjectBindings) error {
	if _, err := s.GetProject(projectID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 Project 绑定事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"project_agent_bindings", "project_skill_bindings"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE project_id = ?", projectID); err != nil {
			return fmt.Errorf("清理 Project 绑定失败: %w", err)
		}
	}
	for _, value := range normalizedBindings(bindings.AgentIDs) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_agent_bindings
			(project_id, agent_id) VALUES (?, ?)`, projectID, value); err != nil {
			return fmt.Errorf("写入 Project Agent 绑定失败: %w", err)
		}
	}
	for _, value := range normalizedBindings(bindings.SkillNames) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_skill_bindings
			(project_id, skill_name) VALUES (?, ?)`, projectID, value); err != nil {
			return fmt.Errorf("写入 Project Skill 绑定失败: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET revision = revision + 1,
		updated_at = ? WHERE id = ?`, time.Now().UTC(), projectID); err != nil {
		return fmt.Errorf("更新 Project 时间失败: %w", err)
	}
	return tx.Commit()
}

// GetProjectBindings 返回稳定排序的 Project 绑定。
func (s *Store) GetProjectBindings(projectID string) (ProjectBindings, error) {
	if _, err := s.GetProject(projectID); err != nil {
		return ProjectBindings{}, err
	}
	result := ProjectBindings{AgentIDs: []string{}, SkillNames: []string{}}
	queries := []struct {
		sql    string
		target *[]string
	}{
		{`SELECT agent_id FROM project_agent_bindings WHERE project_id = ? ORDER BY agent_id`, &result.AgentIDs},
		{`SELECT skill_name FROM project_skill_bindings WHERE project_id = ? ORDER BY skill_name`, &result.SkillNames},
	}
	for _, query := range queries {
		rows, err := s.db.Query(query.sql, projectID)
		if err != nil {
			return ProjectBindings{}, fmt.Errorf("查询 Project 绑定失败: %w", err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				return ProjectBindings{}, fmt.Errorf("扫描 Project 绑定失败: %w", err)
			}
			*query.target = append(*query.target, value)
		}
		if err := rows.Close(); err != nil {
			return ProjectBindings{}, fmt.Errorf("关闭 Project 绑定查询失败: %w", err)
		}
	}
	return result, nil
}

// normalizedBindings 去空、去重并排序，保证绑定替换幂等。
func normalizedBindings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
