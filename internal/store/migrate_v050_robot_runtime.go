package store

import "database/sql"

// migrateV23 保存 Server 在 Pilot 启动前创建的 Runtime Instance 与
// AbilityFramework 端口租约。Pilot 不拥有这些表，Server 重启后仍可对账。
func migrateV23(tx *sql.Tx) error {
	statements := []string{
		"CREATE TABLE robot_runtime_instances (" +
			"instance_id TEXT PRIMARY KEY, robot_id TEXT NOT NULL, status TEXT NOT NULL, " +
			"revision INTEGER NOT NULL, updated_at TIMESTAMP NOT NULL, body BLOB NOT NULL)",
		"CREATE INDEX idx_robot_runtime_robot ON robot_runtime_instances(robot_id, updated_at)",
		"CREATE TABLE robot_runtime_port_leases (" +
			"instance_id TEXT PRIMARY KEY, port INTEGER NOT NULL UNIQUE)",
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}
