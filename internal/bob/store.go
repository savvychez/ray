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
	"strings"
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
	CreatedAt int64   `json:"created_at"`
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

// TaskFor returns the session bob process p is running (or last ran).
// bob 2.0.5 leaves tasks.directory empty, so in order of preference:
//
//  1. the task p was started with (bob -r <id>);
//  2. a task recorded for p's directory, or a folder above or below it;
//  3. a task created after p started, then one updated after p started.
//
// claimed holds task ids other bob processes were started with, which are
// theirs, not p's. Within a rank, a task whose lease is live wins, then
// the most recently updated.
func (s *Store) TaskFor(ctx context.Context, p Proc, claimed map[string]bool) (*Task, error) {
	tasks, err := s.recent(ctx, 500)
	if err != nil {
		return nil, err
	}
	want := cleanDir(p.Dir)
	start := p.Start.Add(-5 * time.Second).UnixMilli() // ps rounds to the second
	var best *Task
	bestRank := 0
	for i := range tasks {
		t := &tasks[i]
		rank := 0
		got := cleanDir(t.Directory)
		switch {
		case p.Resume != "" && t.ID == p.Resume:
			rank = 6
		case p.Resume != "":
			// started on a specific task: only that one (or what bob moved
			// on to since, below) is p's.
		case want != "" && got == want:
			rank = 5
		case want != "" && got != "" && got != "/" && (within(want, got) || within(got, want)):
			rank = 4
		}
		if rank == 0 && !p.Start.IsZero() && !claimed[t.ID] && got == "" {
			switch {
			case t.CreatedAt >= start:
				rank = 3
			case t.UpdatedAt >= start && p.Resume == "":
				rank = 2
			}
		}
		// tasks come live-first, newest-first, so the first at a rank wins.
		if rank > bestRank {
			best, bestRank = t, rank
		}
	}
	return best, nil
}

// RecentTasks lists bob's most recent top-level tasks, live ones first.
func (s *Store) RecentTasks(ctx context.Context, n int) ([]Task, error) {
	return s.recent(ctx, n)
}

func (s *Store) recent(ctx context.Context, n int) ([]Task, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(title, ''), COALESCE(status, ''), COALESCE(directory, ''), COALESCE(costs, ''), created_at, updated_at,
		       (locked_by IS NOT NULL AND COALESCE(lock_lease_until, 0) > ?) AS live
		FROM tasks
		WHERE parent_id IS NULL
		ORDER BY live DESC, updated_at DESC
		LIMIT ?`, now, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var costs string
		if err := rows.Scan(&t.ID, &t.Title, &t.Status, &t.Directory, &costs, &t.CreatedAt, &t.UpdatedAt, &t.Live); err != nil {
			return nil, err
		}
		t.Cost = totalCost(costs)
		out = append(out, t)
	}
	return out, rows.Err()
}

// cleanDir normalizes a directory for comparison: no trailing slash, and
// symlinks resolved (macOS reports /private/tmp for /tmp, for instance).
func cleanDir(d string) string {
	if d == "" {
		return ""
	}
	if c, ok := cleanDirs.Load(d); ok {
		return c.(string)
	}
	c := filepath.Clean(d)
	if r, err := filepath.EvalSymlinks(c); err == nil {
		c = r
	}
	cleanDirs.Store(d, c)
	return c
}

// cleanDirs caches cleanDir, which runs for every task on every poll.
var cleanDirs sync.Map

// within reports whether dir is inside parent.
func within(dir, parent string) bool {
	return strings.HasPrefix(dir, parent+string(filepath.Separator))
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
