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
	"sort"
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

// Hints sharpen Assign: what each bob terminal shows right now, and what
// each was assigned before (so a choice doesn't flicker when the evidence
// scrolls away). Both are keyed by surface id and optional.
type Hints struct {
	Screens map[string]string
	Prev    map[string]string
}

// launchWindow is how soon after starting bob creates its first task (a
// couple of seconds in practice).
const launchWindow = 60 * time.Second

// Assign works out which session each running bob is showing, given the
// bob processes by cmux surface id. bob 2.0.5 leaves tasks.directory
// empty and keeps no lease, so in order of preference:
//
//  1. the task a bob was started with (bob -r <id>);
//  2. a task recorded for its directory, or a folder above or below it;
//  3. by time. bob creates a task as it starts, so a task created just after
//     a bob started is that bob's. Tasks created later (bob's /new) could
//     belong to any bob started before them; the one whose terminal shows
//     the task's messages gets it. Without such evidence a bob keeps its
//     previous task, or else its launch task.
//
// Each task goes to at most one terminal.
func (s *Store) Assign(ctx context.Context, procs map[string]Proc, h Hints) (map[string]*Task, error) {
	tasks, err := s.recent(ctx, 500)
	if err != nil {
		return nil, err
	}
	out := map[string]*Task{}
	taken := map[string]bool{}
	take := func(sf string, t *Task) {
		out[sf] = t
		taken[t.ID] = true
	}
	keys := make([]string, 0, len(procs))
	for sf := range procs {
		keys = append(keys, sf)
	}
	// Newest-started first (ties by id, for a stable answer).
	sort.Slice(keys, func(i, j int) bool {
		a, b := procs[keys[i]].Start, procs[keys[j]].Start
		if !a.Equal(b) {
			return a.After(b)
		}
		return keys[i] < keys[j]
	})

	for _, sf := range keys {
		if id := procs[sf].Resume; id != "" {
			for i := range tasks {
				if tasks[i].ID == id {
					take(sf, &tasks[i])
					break
				}
			}
		}
	}
	for _, sf := range keys {
		p := procs[sf]
		want := cleanDir(p.Dir)
		if out[sf] != nil || want == "" {
			continue
		}
		var best *Task
		bestRank := 0
		for i := range tasks {
			t := &tasks[i]
			got := cleanDir(t.Directory)
			rank := 0
			switch {
			case taken[t.ID] || got == "":
			case got == want:
				rank = 2
			case got != "/" && (within(want, got) || within(got, want)):
				rank = 1
			}
			// tasks come live-first, newest-first, so the first at a rank wins.
			if rank > bestRank {
				best, bestRank = t, rank
			}
		}
		if best != nil {
			take(sf, best)
		}
	}

	// By time. First each bob's launch task: the earliest created just after
	// it started.
	open := func(t *Task) bool { return !taken[t.ID] && t.Directory == "" }
	launch := map[string]*Task{}
	for _, sf := range keys {
		p := procs[sf]
		if out[sf] != nil || p.Start.IsZero() {
			continue
		}
		from := p.Start.Add(-5 * time.Second).UnixMilli() // ps rounds to the second
		to := p.Start.Add(launchWindow).UnixMilli()
		var first *Task
		for i := range tasks {
			t := &tasks[i]
			if open(t) && t.CreatedAt >= from && t.CreatedAt <= to && (first == nil || t.CreatedAt < first.CreatedAt) {
				first = t
			}
		}
		if first != nil {
			launch[sf] = first
			taken[first.ID] = true
		}
	}
	// Then pick between the launch task and later ones.
	for _, sf := range keys {
		p := procs[sf]
		if out[sf] != nil || p.Start.IsZero() {
			continue
		}
		from := p.Start.Add(-5 * time.Second).UnixMilli()
		var cands []*Task
		if l := launch[sf]; l != nil {
			cands = append(cands, l)
		}
		for i := range tasks {
			if t := &tasks[i]; open(t) && t.CreatedAt >= from && len(cands) < 8 {
				cands = append(cands, t) // recent first
			}
		}
		if len(cands) == 0 {
			continue
		}
		pick := s.onScreen(ctx, cands, h.Screens[sf])
		if pick == nil {
			for _, t := range cands {
				if t.ID == h.Prev[sf] {
					pick = t
				}
			}
		}
		if pick == nil {
			pick = cands[0]
		}
		take(sf, pick)
	}
	return out, nil
}

// onScreen picks the task whose recent messages are on a terminal screen,
// or nil if none (or no screen).
func (s *Store) onScreen(ctx context.Context, cands []*Task, screen string) *Task {
	if screen == "" || len(cands) < 2 {
		return nil
	}
	screen = squash(screen)
	var best *Task
	bestScore := 0
	for _, t := range cands {
		rows, err := s.Messages(ctx, t.ID, 0, 20)
		if err != nil {
			continue
		}
		score := 0
		for _, m := range Convert(rows) {
			if m.Role != "user" && m.Role != "assistant" {
				continue
			}
			for _, line := range strings.Split(m.Text, "\n") {
				// A line's start survives the terminal's wrapping.
				if l := squash(line); len(l) >= 12 && strings.Contains(screen, l[:min(len(l), 40)]) {
					score++
				}
			}
		}
		if score > bestScore {
			best, bestScore = t, score
		}
	}
	return best
}

// squash collapses runs of whitespace, for comparing text with a screen.
func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Task returns one task by id, or nil.
func (s *Store) Task(ctx context.Context, id string) (*Task, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, taskQuery+` WHERE id = ?`, time.Now().UnixMilli(), id)
	if err != nil {
		return nil, err
	}
	ts, err := scanTasks(rows)
	if err != nil || len(ts) == 0 {
		return nil, err
	}
	return &ts[0], nil
}

// RecentTasks lists bob's most recent top-level tasks, live ones first.
func (s *Store) RecentTasks(ctx context.Context, n int) ([]Task, error) {
	return s.recent(ctx, n)
}

const taskQuery = `
		SELECT id, COALESCE(title, ''), COALESCE(status, ''), COALESCE(directory, ''), COALESCE(costs, ''), created_at, updated_at,
		       (locked_by IS NOT NULL AND COALESCE(lock_lease_until, 0) > ?) AS live
		FROM tasks`

func (s *Store) recent(ctx context.Context, n int) ([]Task, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, taskQuery+`
		WHERE parent_id IS NULL
		ORDER BY live DESC, updated_at DESC
		LIMIT ?`, time.Now().UnixMilli(), n)
	if err != nil {
		return nil, err
	}
	return scanTasks(rows)
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
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
