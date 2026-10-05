package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
)

// migration 描述一次版本化迁移：version 单调递增，apply 在事务中执行。
// 为什么用版本列表而不是 IF NOT EXISTS 堆叠：版本号让"库当前处于哪个
// schema 状态"可查询、可记录，后续迁移（如 B4 的 chat 表）按序追加即可，
// 旧库升级路径确定，不会出现半新半旧的 schema。
type migration struct {
	version int
	name    string
	apply   func(tx *sql.Tx) error
}

// migrations 是全部迁移的有序列表，必须按 version 升序追加，禁止修改已发布的迁移。
var migrations = []migration{
	{version: 1, name: "create_users_tokens", apply: migrateV1},
	{version: 2, name: "create_trace_metering", apply: migrateV2},
	{version: 3, name: "create_chat_run_sessions", apply: migrateV3},
	{version: 4, name: "create_artifacts_interactions_checkpoint", apply: migrateV4},
	{version: 5, name: "create_events", apply: migrateV5},
	{version: 6, name: "create_settings_keys_audit", apply: migrateV6},
	{version: 7, name: "create_work_sessions_blackboard", apply: migrateV7},
	{version: 8, name: "add_chat_message_runtime_metadata", apply: migrateV8},
	{version: 9, name: "add_artifact_owner", apply: migrateV9},
	{version: 10, name: "create_v014_projects", apply: migrateV10},
	{version: 11, name: "rebuild_chat_messages_with_eino_schema", apply: migrateV11},
	{version: 12, name: "create_session_agent_models", apply: migrateV12},
	{version: 13, name: "create_session_execution_policy", apply: migrateV13},
	{version: 14, name: "create_v020_project_studio_state", apply: migrateV14},
	{version: 15, name: "drop_obsolete_blackboard", apply: migrateV15},
	{version: 16, name: "normalize_replayable_event_sequences", apply: migrateV16},
	{version: 17, name: "create_v030_workflow_semantic_map", apply: migrateV17},
	{version: 18, name: "complete_v030_context_interaction_map", apply: migrateV18},
	{version: 19, name: "add_interaction_routing_idempotency", apply: migrateV19},
	{version: 20, name: "allow_interaction_continuation_attempts", apply: migrateV20},
	{version: 21, name: "create_v040_project_simulation_resources", apply: migrateV21},
	{version: 22, name: "create_v050_robot_execution", apply: migrateV22},
	{version: 23, name: "create_v050_robot_runtime", apply: migrateV23},
	{version: 24, name: "separate_plan_proposal_and_typed_subtasks", apply: migrateV24},
	{version: 25, name: "converge_plan_proposal_states", apply: migrateV25},
	{version: 26, name: "converge_workflow_task_subtask_schema", apply: migrateV26},
	{version: 27, name: "create_pilot_enrollment_and_desired_skills", apply: migrateV27},
	{version: 28, name: "normalize_workflow_conversation_activities", apply: migrateV28},
	{version: 29, name: "reserve_active_robot_per_task", apply: migrateV29},
	{version: 30, name: "converge_workflow_terminal_summary", apply: migrateV30},
	{version: 31, name: "select_runtime_installation_at_scene_start", apply: migrateV31},
	{version: 32, name: "create_trace_span_io", apply: migrateV32},
	{version: 33, name: "converge_trace_io_and_conversation_robot_reservation", apply: migrateV33},
	{version: 34, name: "idempotent_plan_proposal_submission_per_run", apply: migrateV34},
}

func migrateV34(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE plan_proposal_submissions (
		run_id TEXT PRIMARY KEY,
		proposal_id TEXT NOT NULL,
		proposal_revision INTEGER NOT NULL,
		created_at TIMESTAMP NOT NULL,
		FOREIGN KEY(proposal_id) REFERENCES plan_proposals(id)
	)`)
	return err
}

// Both development branches used v32. Keep the published Trace migration and
// converge either already-upgraded database without discarding recorded runs.
func migrateV33(tx *sql.Tx) error {
	if err := migrateV32(tx); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('run_sessions') WHERE name='robot_id'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := tx.Exec(`ALTER TABLE run_sessions ADD COLUMN robot_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_run_sessions_active_robot
		ON run_sessions(robot_id) WHERE robot_id<>'' AND
		status IN ('queued','running','waiting_input','cancelling')`)
	return err
}

// migrateV31 把 Project 从不可更换的 Installation 绑定迁移为可移植 Profile
// 与本机启动偏好。Scene 引用只描述业务场景，不再复制部署位置；活动实例实际
// 使用的 Installation 仍由 ProjectRuntimeState 保存，保证恢复和停止精确路由。
func migrateV31(tx *sql.Tx) error {
	statements := []string{
		`ALTER TABLE projects ADD COLUMN runtime_profile_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN preferred_runtime_installation_id TEXT NOT NULL DEFAULT ''`,
		`UPDATE projects SET preferred_runtime_installation_id=runtime_installation_id
			WHERE runtime_installation_id<>''`,
		`CREATE TABLE project_scene_references_v31 (
			project_scene_id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			catalog_scene_id TEXT NOT NULL,
			scene_version TEXT NOT NULL,
			default_variant_id TEXT NOT NULL DEFAULT '',
			added_at TIMESTAMP NOT NULL,
			UNIQUE(project_id, catalog_scene_id, scene_version)
		)`,
		`INSERT INTO project_scene_references_v31
			(project_scene_id, project_id, catalog_scene_id, scene_version,
			 default_variant_id, added_at)
		 SELECT project_scene_id, project_id, catalog_scene_id, scene_version,
			default_variant_id, added_at FROM project_scene_references`,
		`DROP TABLE project_scene_references`,
		`ALTER TABLE project_scene_references_v31 RENAME TO project_scene_references`,
		`CREATE INDEX idx_project_scene_references_project
			ON project_scene_references(project_id, added_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// migrateV32 把 ChatModel 的输入输出从 trace_spans.attrs 拆到独立表。
// 列表接口只标 has_io，正文按需读取，避免 attrs 膨胀和密钥泄漏。
func migrateV32(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS trace_span_io (
		span_id INTEGER PRIMARY KEY,
		input_text TEXT NOT NULL DEFAULT '',
		output_text TEXT NOT NULL DEFAULT '',
		input_truncated INTEGER NOT NULL DEFAULT 0,
		output_truncated INTEGER NOT NULL DEFAULT 0,
		input_sha256 TEXT NOT NULL DEFAULT '',
		output_sha256 TEXT NOT NULL DEFAULT '',
		FOREIGN KEY(span_id) REFERENCES trace_spans(id) ON DELETE CASCADE
	)`)
	return err
}

// migrateV29 把 Robot 保留落在现有 Task 事实上，而不是另建资源表。pending
// 已分配、running、paused 和 stopping 都可能仍持有物理状态；只有 Task 进入
// 终态后唯一索引才自动释放。这样 Server 重启后也不会把同一 Robot 派给两条 Task。
func migrateV29(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE UNIQUE INDEX idx_tasks_active_robot_reservation
		ON tasks(assigned_robot_id) WHERE assigned_robot_id<>'' AND
		status IN ('pending','running','paused','stopping')`)
	return err
}

// migrateV30 让一个 Workflow 只形成一次终态 Leader 总结。该索引只约束
// Workflow 关联的无 Task Conversation Run，不影响正常对话和 Task Run。
func migrateV30(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE UNIQUE INDEX idx_run_sessions_workflow_terminal_summary
		ON run_sessions(workflow_id) WHERE workflow_id<>'' AND task_id='' AND kind='conversation'`)
	return err
}

// Pilot credential 是设备连接凭据，不复用用户登录 token；desired skill 与
// Pilot 实际上报目录分开保存，Server 才能在重连后继续完成安装对账。
func migrateV27(tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE pilot_enrollments (
			id TEXT PRIMARY KEY, code TEXT NOT NULL UNIQUE, status TEXT NOT NULL,
			expires_at TIMESTAMP NOT NULL, created_by TEXT NOT NULL,
			pilot_instance_id TEXT NOT NULL DEFAULT '', created_at TIMESTAMP NOT NULL,
			claimed_at TIMESTAMP
		)`,
		`CREATE INDEX idx_pilot_enrollments_status_expiry
			ON pilot_enrollments(status,expires_at)`,
		`CREATE TABLE pilot_credentials (
			pilot_instance_id TEXT PRIMARY KEY, credential TEXT NOT NULL UNIQUE,
			created_at TIMESTAMP NOT NULL, revoked_at TIMESTAMP
		)`,
		`CREATE TABLE robot_desired_skills (
			robot_id TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL,
			enabled INTEGER NOT NULL, updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY(robot_id,name)
		)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// migrateV28 一次性修正旧版本写入 Conversation 的 workflow_started system 消息。
// 这类内容是用户可见的 Leader 活动，不是模型系统指令；迁移后运行时只需要
// 接受正常的 user/assistant 对话角色，不保留长期双读兼容。
func migrateV28(tx *sql.Tx) error {
	type roleUpdate struct {
		id          string
		messageJSON string
	}
	rows, err := tx.Query(`SELECT id,message_json,metadata FROM chat_messages`)
	if err != nil {
		return err
	}
	var updates []roleUpdate
	for rows.Next() {
		var id, messageJSON, metadataJSON string
		if err := rows.Scan(&id, &messageJSON, &metadataJSON); err != nil {
			_ = rows.Close()
			return err
		}
		var metadata map[string]any
		if json.Unmarshal([]byte(metadataJSON), &metadata) != nil ||
			metadata["message_kind"] != "workflow_started" {
			continue
		}
		var message schema.Message
		if err := json.Unmarshal([]byte(messageJSON), &message); err != nil {
			_ = rows.Close()
			return fmt.Errorf("解析 Workflow 活动消息 %q 失败: %w", id, err)
		}
		if message.Role != schema.System {
			continue
		}
		message.Role = schema.Assistant
		encoded, err := json.Marshal(&message)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("序列化 Workflow 活动消息 %q 失败: %w", id, err)
		}
		updates = append(updates, roleUpdate{id: id, messageJSON: string(encoded)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := tx.Exec(`UPDATE chat_messages SET message_json=? WHERE id=?`,
			update.messageJSON, update.id); err != nil {
			return err
		}
	}
	return nil
}

// migrateV26 将 Task 的执行者、角色和类型收敛为一组明确字段。旧表中的
// agent_id/kind 只用于本次事务迁移，迁移完成后运行时代码不再双读；同时把
// paused SubTask 保留为需要显式恢复的状态，避免状态未知的物理动作被调度器重放。
func migrateV26(tx *sql.Tx) error {
	statements := []string{
		`ALTER TABLE plan_proposals ADD COLUMN summary TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE workflows ADD COLUMN approved_scope_json TEXT NOT NULL DEFAULT '{}'`,
		`DROP INDEX IF EXISTS idx_subtasks_task_position`,
		`CREATE TABLE subtasks_v26 (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, position INTEGER NOT NULL,
			kind TEXT NOT NULL CHECK(kind IN ('robot_skill','agent_step')),
			goal TEXT NOT NULL, spec_json TEXT NOT NULL DEFAULT '{}',
			criteria_json TEXT NOT NULL DEFAULT '[]',
			evidence_json TEXT NOT NULL DEFAULT '[]', execution_ref TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL, waiting_reason TEXT NOT NULL DEFAULT '',
			result_json TEXT NOT NULL DEFAULT '{}', revision INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL, ended_at TIMESTAMP)`,
		`INSERT INTO subtasks_v26
			(id,task_id,position,kind,goal,spec_json,criteria_json,evidence_json,execution_ref,
			 status,waiting_reason,result_json,revision,created_at,updated_at,ended_at)
		 SELECT s.id,s.task_id,s.position,
			CASE WHEN s.kind IN ('robot','robot_skill') OR
			          COALESCE(NULLIF(TRIM(t.required_role),''),t.kind)='robot'
			     THEN 'robot_skill' ELSE 'agent_step' END,
			s.goal,s.spec_json,s.criteria_json,s.evidence_json,s.execution_ref,s.status,
			s.waiting_reason,s.result_json,s.revision,s.created_at,s.updated_at,s.ended_at
		 FROM subtasks s JOIN tasks t ON t.id=s.task_id`,
		`DROP TABLE subtasks`,
		`ALTER TABLE subtasks_v26 RENAME TO subtasks`,
		`CREATE UNIQUE INDEX idx_subtasks_task_position ON subtasks(task_id,position)`,
		`DROP INDEX IF EXISTS idx_tasks_workflow_position`,
		`DROP INDEX IF EXISTS idx_tasks_workflow_status`,
		`CREATE TABLE tasks_v26 (
			id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, position INTEGER NOT NULL,
			required_role TEXT NOT NULL, required_capabilities_json TEXT NOT NULL DEFAULT '[]',
			resource_requirements_json TEXT NOT NULL DEFAULT '{}',
			assigned_agent_id TEXT NOT NULL DEFAULT '', assigned_robot_id TEXT NOT NULL DEFAULT '',
			assignment_revision INTEGER NOT NULL DEFAULT 0, goal TEXT NOT NULL,
			input_json TEXT NOT NULL DEFAULT '{}', criteria_json TEXT NOT NULL DEFAULT '[]',
			status TEXT NOT NULL, waiting_reason TEXT NOT NULL DEFAULT '',
			result_summary TEXT NOT NULL DEFAULT '', evidence_json TEXT NOT NULL DEFAULT '[]',
			context_id TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL, ended_at TIMESTAMP)`,
		`INSERT INTO tasks_v26
			(id,workflow_id,position,required_role,required_capabilities_json,
			 resource_requirements_json,assigned_agent_id,assigned_robot_id,assignment_revision,
			 goal,input_json,criteria_json,status,waiting_reason,result_summary,evidence_json,
			 context_id,revision,created_at,updated_at,ended_at)
		 SELECT id,workflow_id,position,
			CASE WHEN TRIM(required_role)<>'' THEN required_role
			     WHEN kind IN ('robot','developer','map','monitor') THEN kind
			     WHEN TRIM(agent_id)<>'' THEN agent_id ELSE 'generic' END,
			required_capabilities_json,resource_requirements_json,
			CASE WHEN TRIM(assigned_agent_id)<>'' THEN assigned_agent_id ELSE agent_id END,
			assigned_robot_id,assignment_revision,goal,input_json,criteria_json,status,
			waiting_reason,result_summary,evidence_json,context_id,revision,created_at,updated_at,ended_at
		 FROM tasks`,
		`DROP TABLE tasks`,
		`ALTER TABLE tasks_v26 RENAME TO tasks`,
		`CREATE UNIQUE INDEX idx_tasks_workflow_position ON tasks(workflow_id,position)`,
		`CREATE INDEX idx_tasks_workflow_status ON tasks(workflow_id,status,position)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// migrateV25 删除曾用于异步二次规划的 drafting/failed 状态。Proposal 只有在
// Leader 已经提交完整结构化 Task 后才落库，因此模型流取消或参数纠正都只属于
// Run，不再制造一个需要恢复的半成品计划。SQLite 通过重建表真正移除旧字段，
// 避免运行时代码长期维护双读兼容。
func migrateV25(tx *sql.Tx) error {
	statements := []string{
		`DROP INDEX IF EXISTS idx_plan_proposals_project_active`,
		`DROP INDEX IF EXISTS idx_plan_proposals_conversation`,
		`CREATE TABLE plan_proposals_v25 (
			id                   TEXT PRIMARY KEY,
			project_id           TEXT NOT NULL,
			conversation_id      TEXT NOT NULL,
			revision             INTEGER NOT NULL DEFAULT 1,
			status               TEXT NOT NULL CHECK(status IN ('ready','approved','discarded')),
			goal                 TEXT NOT NULL,
			approved_scope_json  TEXT NOT NULL DEFAULT '{}',
			structured_plan_json TEXT NOT NULL DEFAULT '{}',
			document_markdown    TEXT NOT NULL DEFAULT '',
			created_at           TIMESTAMP NOT NULL,
			updated_at           TIMESTAMP NOT NULL
		)`,
		`INSERT INTO plan_proposals_v25
			(id,project_id,conversation_id,revision,status,goal,approved_scope_json,
			 structured_plan_json,document_markdown,created_at,updated_at)
		 SELECT id,project_id,conversation_id,revision,
			CASE WHEN status IN ('ready','approved','discarded') THEN status ELSE 'discarded' END,
			goal,approved_scope_json,structured_plan_json,document_markdown,created_at,updated_at
		 FROM plan_proposals`,
		`DROP TABLE plan_proposals`,
		`ALTER TABLE plan_proposals_v25 RENAME TO plan_proposals`,
		`CREATE UNIQUE INDEX idx_plan_proposals_project_active
			ON plan_proposals(project_id) WHERE status='ready'`,
		`CREATE INDEX idx_plan_proposals_conversation
			ON plan_proposals(conversation_id, updated_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// migrateV24 把“供用户审阅的计划”从 Workflow 的运行状态中分离出来。
// Plan Proposal 只保存当前 revision；历史内容继续由 Conversation 消息和
// Project 事件承担，避免为了展示历史再复制一套计划快照。Task/SubTask 的
// 新字段均采用兼容默认值，使已有 v0.4/v0.5 数据可以原地升级。
func migrateV24(tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE plan_proposals (
			id                   TEXT PRIMARY KEY,
			project_id           TEXT NOT NULL,
			conversation_id      TEXT NOT NULL,
			revision             INTEGER NOT NULL DEFAULT 1,
			status               TEXT NOT NULL,
			goal                 TEXT NOT NULL,
			approved_scope_json  TEXT NOT NULL DEFAULT '{}',
			structured_plan_json TEXT NOT NULL DEFAULT '{}',
			document_markdown    TEXT NOT NULL DEFAULT '',
			last_error           TEXT NOT NULL DEFAULT '',
			created_at           TIMESTAMP NOT NULL,
			updated_at           TIMESTAMP NOT NULL
		)`,
		`CREATE UNIQUE INDEX idx_plan_proposals_project_active
			ON plan_proposals(project_id)
			WHERE status IN ('drafting','ready','failed')`,
		`CREATE INDEX idx_plan_proposals_conversation
			ON plan_proposals(conversation_id, updated_at DESC)`,
		`ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'generic'`,
		`ALTER TABLE tasks ADD COLUMN required_role TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN required_capabilities_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE tasks ADD COLUMN resource_requirements_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE tasks ADD COLUMN assigned_agent_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN assigned_robot_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN assignment_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE subtasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'generic'`,
		`ALTER TABLE subtasks ADD COLUMN spec_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE subtasks ADD COLUMN criteria_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE subtasks ADD COLUMN evidence_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE subtasks ADD COLUMN execution_ref TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE robot_executions ADD COLUMN subtask_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_robot_executions_subtask
			ON robot_executions(subtask_id, updated_at DESC) WHERE subtask_id<>''`,
		`CREATE TABLE subtask_dependencies (
			task_id                TEXT NOT NULL,
			subtask_id             TEXT NOT NULL,
			depends_on_subtask_id  TEXT NOT NULL,
			PRIMARY KEY(subtask_id, depends_on_subtask_id)
		)`,
		`CREATE INDEX idx_subtask_dependencies_task
			ON subtask_dependencies(task_id, subtask_id)`,
		// v0.3/v0.4 的待审计划曾经伪装成 pending Workflow。新版本不再提供
		// 该状态的执行入口；升级时明确结束这些旧记录，避免它们继续占用
		// Project 的唯一活动 Workflow，同时保留原目标供审计和人工重建。
		`UPDATE workflows SET status='stopped',reason='legacy_plan_replaced',
			ended_at=COALESCE(ended_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP,
			revision=revision+1 WHERE status='pending'`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	// 旧 Task 的 agent_id 表示已经确定的执行者；升级后把它迁入明确的
	// assigned_agent_id，而 required_role 留给新计划按能力后绑定。
	_, err := tx.Exec(`UPDATE tasks SET assigned_agent_id=agent_id
		WHERE agent_id<>'' AND assigned_agent_id=''`)
	return err
}

func migrateV21(tx *sql.Tx) error {
	statements := []string{
		`ALTER TABLE projects ADD COLUMN runtime_installation_id TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE project_scene_references (
			project_scene_id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			catalog_scene_id TEXT NOT NULL,
			scene_version TEXT NOT NULL,
			runtime_installation_id TEXT NOT NULL,
			default_variant_id TEXT NOT NULL DEFAULT '',
			added_at TIMESTAMP NOT NULL,
			UNIQUE(project_id, catalog_scene_id, scene_version)
		)`,
		`CREATE INDEX idx_project_scene_references_project
			ON project_scene_references(project_id, added_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func migrateV20(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX idx_runs_source_interaction`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE UNIQUE INDEX idx_runs_source_interaction_active
		ON run_sessions(source_interaction_id) WHERE source_interaction_id<>'' AND
		status IN ('queued','running','waiting_input','cancelling')`)
	return err
}

func migrateV19(tx *sql.Tx) error {
	statements := []string{
		`ALTER TABLE workflows ADD COLUMN source_interaction_id TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX idx_workflows_source_interaction ON workflows (source_interaction_id) WHERE source_interaction_id<>''`,
		`ALTER TABLE run_sessions ADD COLUMN source_interaction_id TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX idx_runs_source_interaction ON run_sessions (source_interaction_id) WHERE source_interaction_id<>''`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// Migrate 将数据库 schema 迁移到最新版本，重复调用幂等（已应用的迁移自动跳过）。
// 每个迁移在独立事务中执行：任一迁移失败即回滚并报错，库停留在上一个一致版本。
func (s *Store) Migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("创建 schema_migrations 表失败: %w", err)
	}
	if err := s.validateMigrationHistory(); err != nil {
		return err
	}

	for _, m := range migrations {
		applied, err := s.isApplied(m.version)
		if err != nil {
			return err
		}
		if applied {
			s.logger.Debug("迁移已应用，跳过", "version", m.version, "name", m.name)
			continue
		}

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("开启迁移事务失败: %w", err)
		}
		if err := m.apply(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("执行迁移 v%d(%s) 失败: %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.version, m.name,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("记录迁移 v%d(%s) 失败: %w", m.version, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交迁移 v%d(%s) 失败: %w", m.version, m.name, err)
		}
		s.logger.Info("数据库迁移已执行", "version", m.version, "name", m.name)
	}
	return s.ensureMigratedDefaultProjects()
}

// validateMigrationHistory 在执行任何写入迁移前核对版本名称。v0.1.3 曾经
// 占用 v10-v15 创建 Task、ContextEpoch 等实验表；仅比较最大版本会把实验
// v10 误认成当前 v10，因此必须同时核对 name。
func (s *Store) validateMigrationHistory() error {
	expected := make(map[int]string, len(migrations))
	for _, item := range migrations {
		expected[item.version] = item.name
	}
	rows, err := s.db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("读取数据库迁移历史失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return fmt.Errorf("扫描数据库迁移历史失败: %w", err)
		}
		want, ok := expected[version]
		// Only this known v32 branch collision is compatible; v33 converges it.
		if version == 32 && name == "reserve_robot_for_conversation_run" {
			continue
		}
		if !ok || want != name {
			return fmt.Errorf("%w: 数据库迁移 v%d(%s) 不属于当前 schema，请运行 semantic init --reset-data",
				ErrSchemaIncompatible, version, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历数据库迁移历史失败: %w", err)
	}
	return nil
}

// isApplied 查询指定版本的迁移是否已应用。
func (s *Store) isApplied(version int) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT 1 FROM schema_migrations WHERE version = ?`, version,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询迁移版本 v%d 失败: %w", version, err)
	}
	return true, nil
}

// migrateV1 创建认证体系的两张基础表：users（本地账号）与 tokens（访问令牌）。
// chat 等业务表属于后续里程碑，按原则不提前创建。
func migrateV1(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE users (
		id            TEXT PRIMARY KEY,
		username      TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at    TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE tokens (
		token      TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		expires_at TIMESTAMP NOT NULL,
		created_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	return nil
}

// migrateV2 创建内核可观测性的两张表：trace_spans（链路跨度）与
// metering（模型调用计量）。索引只建查询路径上确定会用到的列
// （trace_id 关联查询、model/agent/created_at 过滤），其余按需后补。
func migrateV2(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE trace_spans (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id    TEXT NOT NULL,
		parent_id   TEXT NOT NULL DEFAULT '',
		name        TEXT NOT NULL,
		kind        TEXT NOT NULL,
		started_at  TIMESTAMP NOT NULL,
		duration_ms INTEGER NOT NULL,
		attrs       TEXT NOT NULL DEFAULT '{}'
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_trace_spans_trace_id ON trace_spans (trace_id)`,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE metering (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id          TEXT NOT NULL,
		agent             TEXT NOT NULL DEFAULT '',
		role              TEXT NOT NULL DEFAULT '',
		model             TEXT NOT NULL,
		purpose           TEXT NOT NULL DEFAULT '',
		prompt_tokens     INTEGER NOT NULL,
		completion_tokens INTEGER NOT NULL,
		total_tokens      INTEGER NOT NULL,
		cost_estimate     REAL NOT NULL,
		created_at        TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_metering_model_created ON metering (model, created_at)`,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_metering_agent_created ON metering (agent, created_at)`,
	); err != nil {
		return err
	}
	return nil
}

// migrateV3 创建对话闭环的三张表：chat_sessions（用户对话会话）、
// chat_messages（会话消息，id 按时间近似有序）、run_sessions（每次
// Agent 运行的会话，归档语义，不随会话删除）。
// 索引只建查询路径上确定会用到的列：会话列表按用户、消息按会话 + id 序、
// run 按所属会话。
func migrateV3(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE chat_sessions (
		id         TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		title      TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_chat_sessions_user ON chat_sessions (user_id, updated_at)`,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE chat_messages (
		id         TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		role       TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_chat_messages_session ON chat_messages (session_id, id)`,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE run_sessions (
		id              TEXT PRIMARY KEY,
		agent_name      TEXT NOT NULL,
		chat_session_id TEXT NOT NULL,
		status          TEXT NOT NULL,
		started_at      TIMESTAMP NOT NULL,
		ended_at        TIMESTAMP
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_run_sessions_chat ON run_sessions (chat_session_id)`,
	); err != nil {
		return err
	}
	return nil
}

// migrateV4 创建工具与交互体系的三处 schema 变更：
//   - artifacts：工具产物元数据（内容本体在文件系统，见 artifact.go）；
//   - interactions：结构化交互请求（confirm 审批等），含状态机字段；
//   - run_sessions.checkpoint：run 中断时的内核断点（随 run 存，
//     应答后经断点恢复执行，见 kernel/checkpoint.go）。
//
// 索引只建查询路径上确定会用到的列：交互按会话 + 状态过滤（待应答清单）。
func migrateV4(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE artifacts (
		id         TEXT PRIMARY KEY,
		media_type TEXT NOT NULL DEFAULT '',
		uri        TEXT NOT NULL,
		summary    TEXT NOT NULL DEFAULT '',
		metadata   TEXT NOT NULL DEFAULT '{}',
		size       INTEGER NOT NULL,
		created_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE interactions (
		id            TEXT PRIMARY KEY,
		session_id    TEXT NOT NULL,
		agent         TEXT NOT NULL DEFAULT '',
		type          TEXT NOT NULL,
		status        TEXT NOT NULL,
		payload       TEXT NOT NULL DEFAULT '{}',
		reply         TEXT NOT NULL DEFAULT '',
		run_id        TEXT NOT NULL DEFAULT '',
		checkpoint_id TEXT NOT NULL DEFAULT '',
		created_at    TIMESTAMP NOT NULL,
		answered_at   TIMESTAMP,
		expired_at    TIMESTAMP
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_interactions_session_status ON interactions (session_id, status)`,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE run_sessions ADD COLUMN checkpoint BLOB`); err != nil {
		return err
	}
	return nil
}

// migrateV5 创建事件表 events：聚合器落库的下行事件（envelope 持久化），
// 断连续传（sync 补发）与历史事件视图的唯一事实源。
// 索引只建查询路径上确定会用到的列：补发/历史按会话 + id 序扫描
// （id 字典序即事件发放序，见 aggregate 的事件 ID 设计）。
func migrateV5(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE events (
		id         TEXT PRIMARY KEY,
		session_id TEXT NOT NULL DEFAULT '',
		channel    TEXT NOT NULL,
		type       TEXT NOT NULL,
		importance TEXT NOT NULL,
		agent_id   TEXT NOT NULL DEFAULT '',
		agent_role TEXT NOT NULL DEFAULT '',
		parent     TEXT NOT NULL DEFAULT '{}',
		payload    TEXT NOT NULL DEFAULT '{}',
		ts         TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_events_session_id ON events (session_id, id)`,
	); err != nil {
		return err
	}
	return nil
}

// migrateV6 创建设置体系的两张表：
//   - settings_keys：服务端托管的 API key（name 优先为模型服务 ID，
//     兼容旧端点名），
//     供 settings REST 写入、pkg/llm 注册表在 env 缺失时兜底读取；
//   - settings_audit：设置变更审计（PATCH 配置 / key 增删），
//     只记操作人、动作与变更键清单，绝不记密钥值。
//
// 索引只建查询路径上确定会用到的列：审计按 id 倒序扫描（主键即序）。
func migrateV6(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE settings_keys (
		name       TEXT PRIMARY KEY,
		key_value  TEXT NOT NULL,
		updated_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE settings_audit (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    TEXT NOT NULL DEFAULT '',
		action     TEXT NOT NULL,
		detail     TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	return nil
}

// migrateV7 是已发布数据库的历史迁移，必须保留原内容和版本号，确保旧库
// 能按原顺序升级。Blackboard 运行代码已移除，当前代码不再读写这两张表；
// 后续递增迁移会删除它们，不能修改 v7 来伪造历史迁移结果。
func migrateV7(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE work_sessions (
		id         TEXT PRIMARY KEY,
		team       TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE blackboard_entries (
		id           TEXT PRIMARY KEY,
		work_session TEXT NOT NULL,
		author       TEXT NOT NULL DEFAULT '',
		type         TEXT NOT NULL,
		importance   TEXT NOT NULL,
		payload      TEXT NOT NULL DEFAULT '{}',
		ts           TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX idx_blackboard_entries_ws_ts ON blackboard_entries (work_session, ts)`,
	); err != nil {
		return err
	}
	return nil
}

// migrateV8 为消息补充运行归档字段。run_id 把助手消息与 run_sessions
// 稳定关联；metadata 保存工具调用、委派、用量和终态，使切换页面或重启后
// 仍能完整恢复运行活动，而非依赖浏览器内存中的 WebSocket 增量。
func migrateV8(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE chat_messages ADD COLUMN run_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE chat_messages ADD COLUMN metadata TEXT NOT NULL DEFAULT '{}'`); err != nil {
		return err
	}
	return nil
}

// migrateV9 给用户上传产物增加归属字段，避免图片读取接口跨用户泄漏。
func migrateV9(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE artifacts ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX idx_artifacts_owner_created ON artifacts (owner_id, created_at)`)
	return err
}

// migrateV10 建立 v0.2.0 延续使用的最小 Project 基座。Project 只负责工作区、
// 会话归属和 Agent/Skill 绑定，后续任务运行数据由独立迁移维护。
func migrateV10(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE projects (
		id             TEXT PRIMARY KEY,
		owner_id       TEXT NOT NULL DEFAULT '',
		name           TEXT NOT NULL,
		workspace_root TEXT NOT NULL,
		is_default     INTEGER NOT NULL DEFAULT 0,
		created_at     TIMESTAMP NOT NULL,
		updated_at     TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX idx_projects_owner_updated
		ON projects (owner_id, updated_at)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX idx_projects_owner_default
		ON projects (owner_id) WHERE is_default = 1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE project_agent_bindings (
		project_id TEXT NOT NULL,
		agent_id   TEXT NOT NULL,
		PRIMARY KEY (project_id, agent_id)
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE project_skill_bindings (
		project_id TEXT NOT NULL,
		skill_name TEXT NOT NULL,
		PRIMARY KEY (project_id, skill_name)
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE chat_sessions
		ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX idx_chat_sessions_project_updated
		ON chat_sessions (project_id, updated_at)`)
	return err
}

// migrateV11 将旧的 role/content 双字段消息表重建为 Eino schema.Message
// 信封。迁移会保留稳定基线已有的对话文本和运行活动，但不会把旧表字段继续
// 作为第二套消息语义维护；API 展示字段由读取后的 schema.Message 投影生成。
func migrateV11(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, session_id, role, content, run_id, metadata, created_at
		FROM chat_messages ORDER BY id`)
	if err != nil {
		return err
	}
	type legacyMessage struct {
		id, sessionID, role, content, runID, metadata string
		createdAt                                     time.Time
	}
	var legacy []legacyMessage
	for rows.Next() {
		var item legacyMessage
		if err := rows.Scan(&item.id, &item.sessionID, &item.role, &item.content,
			&item.runID, &item.metadata, &item.createdAt); err != nil {
			_ = rows.Close()
			return err
		}
		legacy = append(legacy, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	if _, err := tx.Exec(`CREATE TABLE chat_messages_v014 (
		id            TEXT PRIMARY KEY,
		session_id    TEXT NOT NULL,
		agent_id      TEXT NOT NULL DEFAULT '',
		run_id        TEXT NOT NULL DEFAULT '',
		trace_id      TEXT NOT NULL DEFAULT '',
		provider      TEXT NOT NULL DEFAULT '',
		endpoint      TEXT NOT NULL DEFAULT '',
		model         TEXT NOT NULL DEFAULT '',
		message_json  TEXT NOT NULL,
		artifact_refs TEXT NOT NULL DEFAULT '[]',
		metadata      TEXT NOT NULL DEFAULT '{}',
		created_at    TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	for _, item := range legacy {
		message := &schema.Message{Role: schema.RoleType(item.role), Content: item.content}
		encoded, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("序列化旧消息 %q 失败: %w", item.id, err)
		}
		if _, err := tx.Exec(`INSERT INTO chat_messages_v014
			(id, session_id, run_id, message_json, metadata, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, item.id, item.sessionID, item.runID,
			string(encoded), item.metadata, item.createdAt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP TABLE chat_messages`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE chat_messages_v014 RENAME TO chat_messages`); err != nil {
		return err
	}
	_, err = tx.Exec(`CREATE INDEX idx_chat_messages_session ON chat_messages (session_id, id)`)
	return err
}

// migrateV12 创建会话级 Agent 模型快照。每个 Agent 在同一会话内只有一条
// 当前配置；Profile 或系统 Default 后续变化不会改写已创建会话。
func migrateV12(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE session_agent_models (
		session_id       TEXT NOT NULL,
		agent_id         TEXT NOT NULL,
		endpoint_id      TEXT NOT NULL,
		reasoning_effort TEXT NOT NULL DEFAULT 'auto',
		source           TEXT NOT NULL,
		created_at       TIMESTAMP NOT NULL,
		updated_at       TIMESTAMP NOT NULL,
		PRIMARY KEY (session_id, agent_id)
	)`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX idx_session_agent_models_agent
		ON session_agent_models (agent_id, updated_at)`)
	return err
}

// migrateV13 保存最小会话执行策略。full 与宿主执行都不会跨会话继承，
// 新会话无记录时由存储层按 ask/false 解释。
func migrateV13(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE session_execution_policies (
		session_id             TEXT PRIMARY KEY,
		mode                   TEXT NOT NULL DEFAULT 'ask',
		host_execution_enabled INTEGER NOT NULL DEFAULT 0,
		updated_at             TIMESTAMP NOT NULL
	)`)
	return err
}

// migrateV14 补齐 v0.2 Studio 主链所需的持久状态。旧的 Project、会话、
// Run、Interaction 和事件数据会原位保留；Run 表采用重建方式一次性完成
// 状态名称收敛，避免后续代码同时理解 done/awaiting_approval 两套取值。
func migrateV14(tx *sql.Tx) error {
	projectChanges := []string{
		`ALTER TABLE projects ADD COLUMN mode TEXT NOT NULL DEFAULT 'development'`,
		`ALTER TABLE projects ADD COLUMN is_active INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE projects ADD COLUMN archived_at TIMESTAMP`,
		`ALTER TABLE projects ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`,
		`CREATE UNIQUE INDEX idx_projects_single_active ON projects (is_active) WHERE is_active = 1`,
		`CREATE INDEX idx_projects_owner_archived_updated ON projects (owner_id, archived_at, updated_at)`,
		`ALTER TABLE chat_sessions ADD COLUMN archived_at TIMESTAMP`,
		`ALTER TABLE chat_sessions ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`,
	}
	for _, statement := range projectChanges {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`CREATE TABLE run_sessions_v020 (
		id              TEXT PRIMARY KEY,
		project_id      TEXT NOT NULL DEFAULT '',
		chat_session_id TEXT NOT NULL,
		context_id      TEXT NOT NULL DEFAULT '',
		agent_id        TEXT NOT NULL DEFAULT '',
		agent_name      TEXT NOT NULL DEFAULT '',
		provider        TEXT NOT NULL DEFAULT '',
		endpoint        TEXT NOT NULL DEFAULT '',
		model           TEXT NOT NULL DEFAULT '',
		trace_id        TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL,
		error           TEXT NOT NULL DEFAULT '',
		checkpoint      BLOB,
		revision        INTEGER NOT NULL DEFAULT 1,
		started_at      TIMESTAMP NOT NULL,
		updated_at      TIMESTAMP NOT NULL,
		ended_at        TIMESTAMP
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO run_sessions_v020
		(id, project_id, chat_session_id, context_id, agent_id, agent_name,
		 status, checkpoint, started_at, updated_at, ended_at)
		SELECT r.id, COALESCE(s.project_id, ''), r.chat_session_id,
		       'leader:' || r.chat_session_id, r.agent_name, r.agent_name,
		       CASE r.status
		         WHEN 'done' THEN 'completed'
		         WHEN 'awaiting_approval' THEN 'waiting_input'
		         WHEN 'running' THEN 'running'
		         WHEN 'cancelled' THEN 'cancelled'
		         WHEN 'failed' THEN 'failed'
		         ELSE 'failed'
		       END,
		       r.checkpoint, r.started_at, COALESCE(r.ended_at, r.started_at), r.ended_at
		FROM run_sessions r
		LEFT JOIN chat_sessions s ON s.id = r.chat_session_id`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE run_sessions`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE run_sessions_v020 RENAME TO run_sessions`); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX idx_run_sessions_chat ON run_sessions (chat_session_id, started_at DESC)`,
		`CREATE INDEX idx_run_sessions_project ON run_sessions (project_id, started_at DESC)`,
		`CREATE INDEX idx_run_sessions_status ON run_sessions (status, updated_at)`,
		`CREATE INDEX idx_run_sessions_trace ON run_sessions (trace_id)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`ALTER TABLE interactions ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE interactions ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE interactions SET project_id = COALESCE(
		(SELECT project_id FROM chat_sessions WHERE chat_sessions.id = interactions.session_id), '')`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX idx_interactions_project_status
		ON interactions (project_id, status, created_at)`); err != nil {
		return err
	}

	if _, err := tx.Exec(`CREATE TABLE context_summaries (
		context_id                 TEXT PRIMARY KEY,
		project_id                 TEXT NOT NULL,
		session_id                 TEXT NOT NULL UNIQUE,
		summary                    TEXT NOT NULL DEFAULT '',
		covered_through_message_id TEXT NOT NULL DEFAULT '',
		revision                   INTEGER NOT NULL DEFAULT 1,
		updated_at                 TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX idx_context_summaries_project
		ON context_summaries (project_id, updated_at)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE project_memories (
		project_id TEXT PRIMARY KEY,
		content    TEXT NOT NULL DEFAULT '',
		revision   INTEGER NOT NULL DEFAULT 1,
		updated_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}

	eventChanges := []string{
		`ALTER TABLE events ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN resource_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN resource_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN sequence INTEGER NOT NULL DEFAULT 0`,
	}
	for _, statement := range eventChanges {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE events SET project_id = COALESCE(
		(SELECT project_id FROM chat_sessions WHERE chat_sessions.id = events.session_id), '')`); err != nil {
		return err
	}
	// 旧事件只在同一 Project 内编号。广播和无法确定 Project 的历史事件
	// 保持 sequence=0，不参与 Studio 的增量恢复。
	if _, err := tx.Exec(`UPDATE events AS current SET sequence = (
		SELECT COUNT(*) FROM events AS previous
		WHERE previous.project_id = current.project_id
		  AND previous.project_id <> ''
		  AND previous.id <= current.id
	) WHERE current.project_id <> ''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE project_event_sequences (
		project_id    TEXT PRIMARY KEY,
		next_sequence INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO project_event_sequences (project_id, next_sequence)
		SELECT project_id, MAX(sequence) + 1 FROM events
		WHERE project_id <> '' GROUP BY project_id`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX idx_events_project_sequence
		ON events (project_id, sequence) WHERE project_id <> '' AND sequence > 0`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX idx_events_project_sequence_scan
		ON events (project_id, sequence)`)
	return err
}

// migrateV15 删除已经退出产品设计和运行主链的 Blackboard 协作表。
// v7 必须继续保留为历史迁移，确保已有数据库能够按原顺序升级；v15 在
// 升级末尾明确清理旧数据，避免后续实现误以为这些表仍是可用的协作接口。
func migrateV15(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP TABLE IF EXISTS blackboard_entries`); err != nil {
		return err
	}
	_, err := tx.Exec(`DROP TABLE IF EXISTS work_sessions`)
	return err
}

// migrateV16 重新整理 Project 事件序号。trace 频道本来就不参与断连续传，
// 因此不能占用 Studio 用来判断缺口的连续 sequence；否则正常的 trace
// 事件会被客户端误判为丢包。旧库中的可恢复事件按原 sequence 压紧，trace
// 事件统一改为 0，并同步下一次分配位置。
func migrateV16(tx *sql.Tx) error {
	if _, err := tx.Exec(`UPDATE events SET sequence = 0
		WHERE project_id <> '' AND channel = ?`, ChannelTrace); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE events AS current SET sequence = (
		SELECT COUNT(*) FROM events AS previous
		WHERE previous.project_id = current.project_id
		  AND previous.channel <> ?
		  AND (previous.sequence < current.sequence OR
		       (previous.sequence = current.sequence AND previous.id <= current.id))
	) WHERE current.project_id <> '' AND current.channel <> ?`,
		ChannelTrace, ChannelTrace); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM project_event_sequences`); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO project_event_sequences (project_id, next_sequence)
		SELECT project_id, COALESCE(MAX(sequence), 0) + 1 FROM events
		WHERE project_id <> '' AND channel <> ? GROUP BY project_id`, ChannelTrace)
	return err
}
