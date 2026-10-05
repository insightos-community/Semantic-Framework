package store

import (
	"testing"
	"time"
)

func TestV33ConvergesBothV32Branches(t *testing.T) {
	for _, branch := range []string{"trace", "robot"} {
		t.Run(branch, func(t *testing.T) {
			st := openTestStore(t)
			// Reconstruct each actually shipped v32 schema in an isolated test DB.
			queries := []string{`DELETE FROM schema_migrations WHERE version=33`}
			if branch == "trace" {
				queries = append(queries, `DROP INDEX idx_run_sessions_active_robot`, `ALTER TABLE run_sessions DROP COLUMN robot_id`)
			} else {
				queries = append(queries, `DROP TABLE trace_span_io`, `UPDATE schema_migrations SET name='reserve_robot_for_conversation_run' WHERE version=32`)
			}
			for _, query := range queries {
				if _, err := st.db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now().UTC()
			if _, err := st.db.Exec(`INSERT INTO run_sessions(id,chat_session_id,status,started_at,updated_at) VALUES('retained-run','cs-retained','completed',?,?)`, started, started); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := st.Migrate(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.GetRunSession("retained-run"); err != nil {
				t.Fatal(err)
			}
			var count int
			for _, query := range []string{`SELECT COUNT(*) FROM pragma_table_info('run_sessions') WHERE name='robot_id'`, `SELECT COUNT(*) FROM sqlite_master WHERE name='trace_span_io'`, `SELECT COUNT(*) FROM sqlite_master WHERE name='idx_run_sessions_active_robot'`} {
				if err := st.db.QueryRow(query).Scan(&count); err != nil || count != 1 {
					t.Fatalf("missing converged schema: count=%d err=%v", count, err)
				}
			}
		})
	}
}
