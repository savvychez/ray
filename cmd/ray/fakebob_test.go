package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/savvychez/ray/internal/bob"
	"github.com/savvychez/ray/internal/server"
)

// fakeBob makes the demo's "agents" terminal look like it runs IBM Bob
// Shell: a bob.db seeded with a session shaped like a real one, and a tiny
// simulator that answers what the phone types and approves.
func fakeBob(t testing.TB, fb *server.FakeBackend) *server.BobWatcher {
	path := filepath.Join(t.TempDir(), "bob.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := bob.CreateSchema(db); err != nil {
		t.Fatal(err)
	}
	tree, _ := fb.Tree(context.Background())
	var sf string
	for _, w := range tree {
		if w.Title == "agents" {
			sf = w.Surfaces[0].ID
		}
	}
	const dir = "/Users/demo/project"
	sim := &bobSim{db: db}
	now := time.Now().UnixMilli()
	sim.exec(`INSERT INTO tasks (id, project_id, title, directory, created_at, updated_at, locked_by, lock_lease_until, costs)
		VALUES ('task1', 'p', 'Test permissions', ?, ?, ?, 'sess', ?, '{"totalCost":0.42}')`, dir, now, now, now+3600_000)
	sim.add("user", map[string]any{"content": "want to test out todo list, linux, and file editing permissions from u. can u send reqs for all of those"})
	sim.add("assistant", map[string]any{"content": "", "toolCalls": []any{map[string]any{"id": "c1", "name": "update_todo_list", "arguments": map[string]any{"todos": "[x] Test todo list functionality\n[-] Test Linux command execution\n[ ] Test file editing permissions"}}}})
	sim.add("tool", map[string]any{"content": "Todo list updated", "toolUsage": map[string]any{"signature": map[string]any{"id": "c1", "name": "update_todo_list"}, "labels": map[string]any{"displayName": "Update Todo List"}}})
	sim.add("assistant", map[string]any{"content": "", "toolCalls": []any{map[string]any{"id": "c2", "name": "execute_command", "arguments": map[string]any{"command": "uname -a && whoami && ls -la"}}}})
	sim.add("tool", map[string]any{"content": "Darwin Sarveshs-MacBook-Pro 25.0.0 arm64\nschez\ntotal 24\ndrwxr-xr-x  5 schez staff 160 Oct  7 15:20 .", "toolUsage": map[string]any{"signature": map[string]any{"id": "c2", "name": "execute_command"}, "labels": map[string]any{"displayName": "Execute Command"}}})
	sim.add("assistant", map[string]any{"content": "Linux command execution works (**macOS/Darwin** here, not Linux, but shell access is confirmed). Now testing file editing:\n\n- write a scratch file\n- edit it with `search_and_replace`\n- remove it", "_meta": map[string]any{"spend": map[string]any{"cost": 0.42}}})

	fb.OnInput = func(surface, kind, v string) {
		if surface == sf {
			sim.input(kind, v)
		}
	}
	return &server.BobWatcher{Store: &bob.Store{Path: path}, Detect: bob.StaticDetector{"fake-" + sf: dir}}
}

type bobSim struct {
	db    *sql.DB
	mu    sync.Mutex
	n     int
	typed string
	downs int
	call  int
}

func (s *bobSim) exec(q string, args ...any) {
	if _, err := s.db.Exec(q, args...); err != nil {
		panic(err)
	}
}

func (s *bobSim) add(role string, data map[string]any) {
	s.n++
	data["role"] = role
	b, _ := json.Marshal(data)
	s.exec(`INSERT INTO messages (id, task_id, role, data, created_at) VALUES (?, 'task1', ?, ?, ?)`, fmt.Sprint("m", s.n), role, string(b), time.Now().UnixMilli())
}

func (s *bobSim) pending() bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM task_pending_approvals`).Scan(&n)
	return n > 0
}

func (s *bobSim) input(kind, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case kind == "text":
		s.typed += strings.TrimRight(v, "\r\n")
	case kind == "key" && v == "down":
		s.downs++
	case kind == "key" && v == "enter" && s.pending():
		// Answer the approval menu: Approve Once / Always / Reject.
		choice := []string{"approved", "approved (always for this task)", "rejected"}[min(s.downs, 2)]
		s.downs = 0
		s.exec(`DELETE FROM task_pending_approvals`)
		id := fmt.Sprint("x", s.call)
		if choice == "rejected" {
			s.add("tool", map[string]any{"content": "The user rejected this operation.", "toolUsage": map[string]any{"signature": map[string]any{"id": id, "name": "execute_command", "isError": true}, "labels": map[string]any{"displayName": "Execute Command"}}})
			go s.later(800*time.Millisecond, func() { s.add("assistant", map[string]any{"content": "OK, I won't run that. Anything else?"}) })
			return
		}
		s.add("tool", map[string]any{"content": "hello from the demo", "toolUsage": map[string]any{"signature": map[string]any{"id": id, "name": "execute_command"}, "labels": map[string]any{"displayName": "Execute Command"}}})
		go s.later(900*time.Millisecond, func() {
			s.add("assistant", map[string]any{"content": "Ran it — the command was " + choice + ". It printed `hello from the demo`."})
		})
	case kind == "key" && v == "enter":
		msg := strings.TrimSpace(s.typed)
		s.typed = ""
		if msg == "" {
			return
		}
		s.add("user", map[string]any{"content": msg})
		s.call++
		id := fmt.Sprint("x", s.call)
		go s.later(700*time.Millisecond, func() {
			s.add("assistant", map[string]any{"content": "I'll run a command for that.", "toolCalls": []any{map[string]any{"id": id, "name": "execute_command", "arguments": map[string]any{"command": "echo hello from the demo"}}}})
			s.exec(`INSERT INTO task_pending_approvals (task_id, request_id, payload_json, created_at) VALUES ('task1', ?, '{}', ?)`, "r"+id, time.Now().UnixMilli())
		})
	}
}

func (s *bobSim) later(d time.Duration, f func()) {
	time.Sleep(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}
