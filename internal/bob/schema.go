package bob

import "database/sql"

// CreateSchema creates the subset of bob's tables ray reads, matching
// bobshell 2.0.5. Only tests and the demo use it; ray never writes to a
// real bob database.
func CreateSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT REFERENCES tasks(id),
			title TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active', first_message TEXT,
			directory TEXT NOT NULL, version TEXT, git_sha TEXT, git_branch TEXT, env TEXT, costs TEXT,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, time_archived INTEGER,
			locked_by TEXT, lock_lease_until INTEGER);
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			role TEXT NOT NULL, data TEXT NOT NULL, created_at INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS task_pending_approvals (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, request_id TEXT NOT NULL,
			payload_json TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (task_id, request_id));`)
	return err
}
