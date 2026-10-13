package server

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/savvychez/ray/internal/bob"
	"github.com/savvychez/ray/internal/push"
)

type fakePusher struct {
	mu   sync.Mutex
	sent []push.Notification
}

func (f *fakePusher) PublicKey() (string, error)                 { return "key", nil }
func (f *fakePusher) Subscribe(dev, name string, b []byte) error { return nil }
func (f *fakePusher) Unsubscribe(dev string) error               { return nil }
func (f *fakePusher) Send(ctx context.Context, n push.Notification, skip func(string) bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if skip("phone") {
		return 0, nil
	}
	f.sent = append(f.sent, n)
	return 1, nil
}

func (f *fakePusher) take() []push.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sent
	f.sent = nil
	return s
}

func TestNotifyBob(t *testing.T) {
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
	exec(`INSERT INTO tasks (id, project_id, title, directory, created_at, updated_at) VALUES ('t1', 'p', 'check the os', '/w', ?, ?)`, now, now)
	n := 0
	addMsg := func(role, data string) {
		n++
		exec(`INSERT INTO messages (id, task_id, role, data, created_at) VALUES (?, 't1', ?, ?, ?)`, fmt.Sprint("m", n), role, data, now)
	}

	fb := NewFakeBackend()
	tree, _ := fb.Tree(context.Background())
	ws, sf := tree[1].ID, tree[1].Surfaces[0].ID
	pu := &fakePusher{}
	s := &Server{
		Backend: fb,
		Bob:     &BobWatcher{Store: &bob.Store{Path: path}, Detect: bob.StaticDetector{"fake-" + sf: "/w"}},
		Push:    pu,
		Logf:    t.Logf,
	}
	defer s.Bob.Store.Close()
	seen := map[string]*bobSeen{}
	ctx := context.Background()
	poll := func() []push.Notification {
		t.Helper()
		s.Bob.assignAt = time.Time{} // don't wait for the assignment cache
		s.notifyPoll(ctx, seen)
		return pu.take()
	}

	addMsg("user", `{"role":"user","content":"what os is this"}`)
	if got := poll(); len(got) != 0 {
		t.Fatalf("first sight notified: %+v", got)
	}

	// Permission prompt on screen.
	addMsg("assistant", `{"role":"assistant","content":"","toolCalls":[{"id":"c1","name":"execute_command","arguments":{"command":"uname"}}]}`)
	fb.SetScreen(ws, sf, "────────────────────\nExecute Command\n────────────────────\nCommand: uname\n→ Approve Once\n  Always Allow Command for task\n  Reject\n↑↓ (1/3)")
	got := poll()
	if len(got) != 1 || got[0].Title != "bob needs approval · "+tree[1].Title || got[0].Body != "Execute Command: Command: uname" || got[0].SF != sf {
		t.Fatalf("approval: %+v", got)
	}
	if got := poll(); len(got) != 0 {
		t.Fatalf("same prompt notified twice: %+v", got)
	}

	// Approved: bob runs the tool, then answers.
	fb.SetScreen(ws, sf, "$ ")
	addMsg("tool", `{"role":"tool","content":"Darwin","toolUsage":{"signature":{"id":"c1","name":"execute_command"}}}`)
	if got := poll(); len(got) != 0 {
		t.Fatalf("working notified: %+v", got)
	}
	addMsg("assistant", `{"role":"assistant","content":"It's   macOS\n(Darwin)."}`)
	if got := poll(); len(got) != 0 {
		t.Fatalf("done after one idle poll (could be between tool calls): %+v", got)
	}
	got = poll()
	if len(got) != 1 || got[0].Title != "bob is done · "+tree[1].Title || got[0].Body != "It's macOS (Darwin)." {
		t.Fatalf("done: %+v", got)
	}
	if got := poll(); len(got) != 0 {
		t.Fatalf("done notified twice: %+v", got)
	}

	// A phone with that terminal open isn't notified.
	ss := &session{device: "phone", watchSF: sf, lastRx: time.Now()}
	s.sessions = map[*session]struct{}{ss: {}}
	addMsg("user", `{"role":"user","content":"again"}`)
	poll()
	addMsg("assistant", `{"role":"assistant","content":"Still macOS."}`)
	poll()
	if got := poll(); len(got) != 0 {
		t.Fatalf("notified a phone that's watching: %+v", got)
	}
}
