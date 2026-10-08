// Package bob reads IBM Bob Shell's session store so ray can show a running
// bob session as a chat instead of a terminal.
//
// Bob keeps every task (session) in a SQLite database, ~/.bob/db/bob.db:
//
//	tasks(id, title, status, directory, costs, updated_at, locked_by, lock_lease_until, …)
//	messages(id, task_id, role, data JSON, created_at)
//	task_pending_approvals(task_id, request_id, payload_json, created_at)
//
// The interactive `bob chat` writes each message as it happens and holds a
// lease on its task (locked_by / lock_lease_until) while it runs. ray only
// ever opens the database read-only.
package bob

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultPath is where bob keeps its database.
func DefaultPath() string {
	if p := os.Getenv("RAY_BOB_DB"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".bob", "db", "bob.db")
}

// Store is a read-only view of a bob database.
type Store struct {
	Path string

	mu sync.Mutex
	db *sql.DB
}

// Task is one bob session.
type Task struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Status    string  `json:"status"`
	Directory string  `json:"directory"`
	Cost      float64 `json:"cost,omitempty"`
	Live      bool    `json:"live"` // a bob process holds its lease
	UpdatedAt int64   `json:"updated_at"`
}

// Row is a raw message row. Seq is SQLite's rowid, which grows as bob
// appends, so it doubles as a cursor for "messages since".
type Row struct {
	Seq       int64
	ID        string
	Role      string
	Data      string
	CreatedAt int64
}

// Approval is a permission prompt bob is waiting on.
type Approval struct {
	RequestID string          `json:"request_id"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt int64           `json:"created_at"`
}

// ErrNoDB means bob has never run on this machine.
var ErrNoDB = errors.New("no bob database")

func (s *Store) open() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, nil
	}
	if _, err := os.Stat(s.Path); err != nil {
		return nil, ErrNoDB
	}
	u := url.URL{Scheme: "file", Path: s.Path, RawQuery: "mode=ro&_pragma=busy_timeout(3000)&_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	s.db = db
	return db, nil
}

// Close releases the database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// TaskFor returns the session bob is running (or last ran) in dir:
// prefer a task whose lease is live, then the most recently updated.
func (s *Store) TaskFor(ctx context.Context, dir string) (*Task, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	row := db.QueryRowContext(ctx, `
		SELECT id, title, status, directory, COALESCE(costs, ''), updated_at,
		       (locked_by IS NOT NULL AND COALESCE(lock_lease_until, 0) > ?) AS live
		FROM tasks
		WHERE directory = ? AND parent_id IS NULL
		ORDER BY live DESC, updated_at DESC
		LIMIT 1`, now, dir)
	var t Task
	var costs string
	if err := row.Scan(&t.ID, &t.Title, &t.Status, &t.Directory, &costs, &t.UpdatedAt, &t.Live); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	t.Cost = totalCost(costs)
	return &t, nil
}

// totalCost pulls a dollar total out of bob's costs JSON, whose exact shape
// isn't documented: a number, or an object with a total/cost field.
func totalCost(s string) float64 {
	if s == "" {
		return 0
	}
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return 0
	}
	switch c := v.(type) {
	case float64:
		return c
	case map[string]any:
		for _, k := range []string{"totalCost", "total", "cost"} {
			if f, ok := c[k].(float64); ok {
				return f
			}
		}
	}
	return 0
}

// Messages returns the task's messages after seq (0 for the latest), oldest
// first, at most limit of them (the newest ones if there are more).
func (s *Store) Messages(ctx context.Context, taskID string, after int64, limit int) ([]Row, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT rowid, id, role, data, created_at FROM messages
		WHERE task_id = ? AND rowid > ?
		ORDER BY rowid DESC LIMIT ?`, taskID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Seq, &r.ID, &r.Role, &r.Data, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	// Reverse into chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// Approvals returns the permission prompts the task is waiting on.
func (s *Store) Approvals(ctx context.Context, taskID string) ([]Approval, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT request_id, payload_json, created_at FROM task_pending_approvals
		WHERE task_id = ? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		var p string
		if err := rows.Scan(&a.RequestID, &p, &a.CreatedAt); err != nil {
			return nil, err
		}
		if json.Valid([]byte(p)) {
			a.Payload = json.RawMessage(p)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
