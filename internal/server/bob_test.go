package server

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/savvychez/ray/internal/bob"
)

func TestBobChat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bob.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := bob.CreateSchema(db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	exec(`INSERT INTO tasks (id, project_id, title, directory, created_at, updated_at, locked_by, lock_lease_until) VALUES ('t1', 'p', 'test perms', '/w', ?, ?, 'sess', ?)`, now, now, now+600000)
	n := 0
	addMsg := func(role, data string) {
		n++
		exec(`INSERT INTO messages (id, task_id, role, data, created_at) VALUES (?, 't1', ?, ?, ?)`, "m"+string(rune('a'+n)), role, data, time.Now().UnixMilli())
	}
	addMsg("user", `{"role":"user","content":"run uname"}`)

	fb := NewFakeBackend()
	tree, _ := fb.Tree(context.Background())
	ws, sf := tree[1].ID, tree[1].Surfaces[0].ID // the "agents" workspace's terminal
	var mu sync.Mutex
	var keys []string
	fb.OnInput = func(s, kind, v string) {
		if s == sf && kind == "key" {
			mu.Lock()
			keys = append(keys, v)
			mu.Unlock()
		}
	}
	w := &BobWatcher{Store: &bob.Store{Path: path}, Detect: bob.StaticDetector{"fake-" + sf: "/w"}}
	defer w.Store.Close()
	send, frames := pipeSession(t, fb, func(s *Server) { s.Bob = w })

	next := func(pred func(Msg) bool) Msg {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case m, ok := <-frames:
				if !ok {
					t.Fatal("session closed")
				}
				if pred(m) {
					return m
				}
			case <-timeout:
				t.Fatal("timed out waiting for frame")
			}
		}
	}
	isChat := func(m Msg) bool { return m.T == "chat" && m.Chat != nil }

	treeMsg := next(func(m Msg) bool { return m.T == "tree" })
	if !treeMsg.Workspaces[1].Surfaces[0].Bob || treeMsg.Workspaces[0].Surfaces[0].Bob {
		t.Fatalf("bob flag wrong in tree: %+v", treeMsg.Workspaces)
	}

	send(Msg{T: "watch", WS: ws, SF: sf})
	first := next(isChat).Chat
	if !first.Reset || first.Task == nil || first.Task.Title != "test perms" || len(first.Msgs) != 1 || first.State != bob.StateWorking {
		t.Fatalf("first chat frame = %+v", first)
	}

	// Approve with nothing pending: refused, no keys pressed.
	send(Msg{T: "approve", ID: 5, WS: ws, SF: sf, Data: "once"})
	if a := next(func(m Msg) bool { return m.T == "ack" && m.ID == 5 }); !strings.Contains(a.Err, "isn't waiting") {
		t.Fatalf("approve without prompt: ack = %+v", a)
	}

	// bob calls a tool and asks for permission.
	addMsg("assistant", `{"role":"assistant","content":"","toolCalls":[{"id":"c1","name":"execute_command","arguments":{"command":"uname -a"}}]}`)
	exec(`INSERT INTO task_pending_approvals (task_id, request_id, payload_json, created_at) VALUES ('t1', 'r1', '{"x":1}', ?)`, now)
	f := next(func(m Msg) bool { return isChat(m) && m.Chat.State == bob.StateApproval }).Chat
	if f.Reset || f.Approval == nil || f.Approval.Call == nil || f.Approval.Call.ID != "c1" {
		t.Fatalf("approval frame = %+v", f)
	}

	send(Msg{T: "approve", ID: 6, WS: ws, SF: sf, Data: "reject"})
	if a := next(func(m Msg) bool { return m.T == "ack" && m.ID == 6 }); a.Err != "" {
		t.Fatalf("approve: %+v", a)
	}
	mu.Lock()
	got := strings.Join(keys, ",")
	mu.Unlock()
	if got != "down,down,enter" {
		t.Fatalf("reject keys = %q", got)
	}

	// The tool runs and bob answers: incremental frame, state back to idle.
	exec(`DELETE FROM task_pending_approvals`)
	addMsg("tool", `{"role":"tool","content":"Darwin","toolUsage":{"signature":{"id":"c1","name":"execute_command"},"labels":{"displayName":"Execute Command"}}}`)
	addMsg("assistant", `{"role":"assistant","content":"It's macOS."}`)
	f = next(func(m Msg) bool { return isChat(m) && m.Chat.State == bob.StateIdle }).Chat
	if f.Reset || f.Approval != nil || len(f.Msgs) == 0 || f.Msgs[len(f.Msgs)-1].Text != "It's macOS." {
		t.Fatalf("final frame = %+v", f)
	}

	// bob 2.0.5 only draws its permission menu; nothing in the store.
	addMsg("assistant", `{"role":"assistant","content":"","toolCalls":[{"id":"c2","name":"execute_command","arguments":{"command":"date"}}]}`)
	menu := "────────────────────\nExecute Command\n────────────────────\nCommand: date\n%s Approve Once\n%s Always Allow Command for task\n%s Reject\n↑↓ (1/3)\nPress Enter to confirm"
	fb.SetScreen(ws, sf, fmt.Sprintf(menu, "→", " ", " "))
	f = next(func(m Msg) bool { return isChat(m) && m.Chat.State == bob.StateApproval }).Chat
	if f.Approval == nil || f.Approval.Prompt == nil || f.Approval.Prompt.Title != "Execute Command" || f.Approval.Call == nil || f.Approval.Call.ID != "c2" {
		t.Fatalf("screen approval frame = %+v", f.Approval)
	}
	// Someone moved the cursor to Reject at the laptop: "always" goes up one.
	fb.SetScreen(ws, sf, fmt.Sprintf(menu, " ", " ", "→"))
	mu.Lock()
	keys = nil
	mu.Unlock()
	send(Msg{T: "approve", ID: 7, WS: ws, SF: sf, Data: "always"})
	if a := next(func(m Msg) bool { return m.T == "ack" && m.ID == 7 }); a.Err != "" {
		t.Fatalf("approve from screen: %+v", a)
	}
	mu.Lock()
	got = strings.Join(keys, ",")
	mu.Unlock()
	if got != "up,enter" {
		t.Fatalf("always keys = %q", got)
	}
}
