package bob

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture builds a bob.db with bob's schema (from bobshell 2.0.5) for tests
// and the demo. It returns a writable handle for adding rows.
func Fixture(t testing.TB) (path string, db *sql.DB) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "bob.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := CreateSchema(db); err != nil {
		t.Fatal(err)
	}
	return path, db
}

const (
	userMsg   = `{"role":"user","content":"want to test out todo list, linux, and file editing permissions","envContext":"<environment_details>…</environment_details>","availableTools":["execute_command"]}`
	callMsg   = `{"role":"assistant","content":"","id":"a1","_meta":{"timestamp":1791404410259,"spend":{"cost":0.33}},"toolCalls":[{"id":"call_1","name":"execute_command","arguments":{"command":"uname -a && whoami && ls -la"}}]}`
	resultMsg = `{"role":"tool","content":"Darwin mbp 25.0\nschez","toolUsage":{"signature":{"id":"call_1","name":"execute_command","arguments":{"command":"uname -a"},"isError":false},"permission":"execute","labels":{"displayName":"Execute Command","success":"Ran"}}}`
	answerMsg = `{"role":"assistant","content":"Shell access works. **Next** I'll test file editing.","id":"a2","_meta":{"timestamp":1791404420088,"spend":{"cost":0.34}}}`
)

func TestStore(t *testing.T) {
	path, db := Fixture(t)
	now := time.Now().UnixMilli()
	mustExec(t, db, `INSERT INTO tasks (id, project_id, title, status, directory, created_at, updated_at, locked_by, lock_lease_until)
		VALUES ('old', 'p', 'old task', 'active', '/w', 1, 50, NULL, NULL),
		       ('live', 'p', 'live task', 'active', '/w', 2, 10, 'sess', ?),
		       ('other', 'p', 'elsewhere', 'active', '/x', 3, 99, NULL, NULL)`, now+60000)
	for i, m := range []struct{ role, data string }{{"user", userMsg}, {"assistant", callMsg}, {"tool", resultMsg}, {"assistant", answerMsg}} {
		mustExec(t, db, `INSERT INTO messages (id, task_id, role, data, created_at) VALUES (?, 'live', ?, ?, ?)`, "m"+itoa(i), m.role, m.data, now)
	}
	mustExec(t, db, `INSERT INTO task_pending_approvals (task_id, request_id, payload_json, created_at) VALUES ('live', 'r1', '{"tool":"execute_command"}', ?)`, now)

	s := &Store{Path: path}
	defer s.Close()
	ctx := context.Background()

	task, err := s.TaskFor(ctx, "/w")
	if err != nil || task == nil || task.ID != "live" || !task.Live {
		t.Fatalf("TaskFor /w = %+v, %v; want the live task over the more recently updated one", task, err)
	}
	if task, _ := s.TaskFor(ctx, "/nowhere"); task != nil {
		t.Fatalf("TaskFor /nowhere = %+v", task)
	}

	rows, err := s.Messages(ctx, "live", 0, 100)
	if err != nil || len(rows) != 4 {
		t.Fatalf("Messages = %d rows, %v", len(rows), err)
	}
	more, _ := s.Messages(ctx, "live", rows[1].Seq, 100)
	if len(more) != 2 || more[0].Role != "tool" {
		t.Fatalf("Messages after seq: %+v", more)
	}
	tail, _ := s.Messages(ctx, "live", 0, 2)
	if len(tail) != 2 || tail[1].Seq != rows[3].Seq {
		t.Fatalf("limit should keep the newest rows: %+v", tail)
	}

	msgs := Convert(rows)
	if msgs[0].Role != "user" || !strings.HasPrefix(msgs[0].Text, "want to test") {
		t.Errorf("user msg = %+v", msgs[0])
	}
	if c := msgs[1].Calls; len(c) != 1 || c[0].Name != "execute_command" || !strings.Contains(string(c[0].Args), "uname") {
		t.Errorf("tool call = %+v", msgs[1])
	}
	if r := msgs[2].Res; r == nil || r.CallID != "call_1" || r.Display != "Execute Command" || !strings.Contains(r.Output, "Darwin") {
		t.Errorf("tool result = %+v", msgs[2].Res)
	}
	if msgs[1].Time != 1791404410259 || msgs[1].Cost != 0.33 {
		t.Errorf("meta not used: %+v", msgs[1])
	}

	apps, err := s.Approvals(ctx, "live")
	if err != nil || len(apps) != 1 || apps[0].RequestID != "r1" {
		t.Fatalf("Approvals = %+v, %v", apps, err)
	}
	if st := StateOf(task, msgs, apps); st != StateApproval {
		t.Errorf("state with approval = %s", st)
	}
	if st := StateOf(task, msgs, nil); st != StateIdle {
		t.Errorf("state after a plain answer = %s", st)
	}
	if st := StateOf(task, msgs[:2], nil); st != StateWorking {
		t.Errorf("state while a tool runs = %s", st)
	}
	if st := StateOf(&Task{Live: false}, msgs, nil); st != StateStopped {
		t.Errorf("state without a live lease = %s", st)
	}
}

func TestStoreMissingDB(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "nope.db")}
	if _, err := s.TaskFor(context.Background(), "/w"); err != ErrNoDB {
		t.Fatalf("err = %v, want ErrNoDB", err)
	}
}

func TestConvertTrimsAndSurvivesJunk(t *testing.T) {
	big := strings.Repeat("x", 50000)
	b, _ := json.Marshal(map[string]any{"role": "assistant", "content": "", "toolCalls": []any{map[string]any{"id": "c", "name": "write_file", "arguments": map[string]any{"path": "/a", "content": big}}}})
	msgs := Convert([]Row{{Seq: 1, Role: "assistant", Data: string(b)}, {Seq: 2, Role: "info", Data: "not json"}})
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs", len(msgs))
	}
	if len(msgs[0].Calls[0].Args) > maxArg+200 {
		t.Errorf("args not trimmed: %d bytes", len(msgs[0].Calls[0].Args))
	}
	if msgs[1].Text != "not json" {
		t.Errorf("junk row = %+v", msgs[1])
	}
	// Content as a list of parts.
	parts := Convert([]Row{{Seq: 3, Role: "user", Data: `{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image"}]}`}})
	if parts[0].Text != "hi[image]" {
		t.Errorf("parts = %q", parts[0].Text)
	}
}

func TestIsBobCommand(t *testing.T) {
	yes := [][]string{{"bob"}, {"/opt/homebrew/bin/bob", "chat"}, {"node", "/usr/local/bin/bob", "chat"}, {"node", "/x/bobshell/dist/bob.js"}}
	no := [][]string{{"python3", "scripts/bob-status.py"}, {"-zsh"}, {"vim", "bob"}, {"node", "server.js", "bob"}}
	for _, a := range yes {
		if !IsBobCommand(a) {
			t.Errorf("IsBobCommand(%q) = false", a)
		}
	}
	for _, a := range no {
		if IsBobCommand(a) {
			t.Errorf("IsBobCommand(%q) = true", a)
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestParsePS(t *testing.T) {
	out := []byte(`  101 ttys012  -zsh
  202 ttys012  node /opt/homebrew/bin/bob chat --auto-approve
  203 ttys012  /opt/homebrew/Cellar/node/22.1/bin/node --max-old-space-size=8192 /opt/homebrew/lib/node_modules/bobshell/dist/bob.js chat
  303 s013     bob
  404 ??       node /x/bob.js
  505 ttys014  python3 scripts/bob-status.py --interval 1
  606 pts/3    vim bob.txt
`)
	got := parsePS(out)
	var pids []string
	for _, p := range got {
		pids = append(pids, p.PID+"@"+p.TTY)
	}
	want := "202@ttys012 203@ttys012 303@s013 404@"
	if strings.Join(pids, " ") != want {
		t.Fatalf("parsePS = %q, want %q", strings.Join(pids, " "), want)
	}
}

func TestProcDetectorMatching(t *testing.T) {
	d := &ProcDetector{TTL: time.Hour, at: time.Now(), procs: []Proc{
		{PID: "1", TTY: "", Dir: "/w/env", Surface: "AB57-SURFACE-0000-0000-000000000000"},
		{PID: "2", TTY: "s013", Dir: "/w/tty"},
		{PID: "3", TTY: "ttys020", Dir: ""}, // cwd unknown: ignored
	}}
	ctx := context.Background()
	if dir, ok := d.Lookup(ctx, "ab57-surface-0000-0000-000000000000", ""); !ok || dir != "/w/env" {
		t.Errorf("by surface env: %q %v", dir, ok)
	}
	if dir, ok := d.Lookup(ctx, "other", "/dev/ttys013"); !ok || dir != "/w/tty" {
		t.Errorf("by tty (short ps form): %q %v", dir, ok)
	}
	if _, ok := d.Lookup(ctx, "other", "ttys020"); ok {
		t.Errorf("process without a cwd matched")
	}
	if _, ok := d.Lookup(ctx, "nobody", ""); ok {
		t.Errorf("unknown surface matched")
	}
}

func TestSurfaceEnvRegexp(t *testing.T) {
	// macOS `ps -E` appends the environment after the command.
	line := []byte("node /opt/homebrew/bin/bob chat TERM=xterm-ghostty CMUX_WORKSPACE_ID=11111111-2222-3333-4444-555555555555 CMUX_SURFACE_ID=6f11d59d-b987-4475-8730-d05e83b501fe HOME=/Users/x")
	m := surfaceEnv.FindSubmatch(line)
	if m == nil || strings.ToUpper(string(m[1])) != "6F11D59D-B987-4475-8730-D05E83B501FE" {
		t.Fatalf("got %q", m)
	}
	// Linux /proc/*/environ is NUL-separated.
	if surfaceEnv.FindSubmatch([]byte("A=1\x00CMUX_SURFACE_ID=6f11d59d-b987-4475-8730-d05e83b501fe\x00")) == nil {
		t.Fatal("NUL-separated environ not matched")
	}
	// Not a different variable that merely ends in the name.
	if surfaceEnv.FindSubmatch([]byte("OLD_CMUX_SURFACE_ID=6f11d59d-b987-4475-8730-d05e83b501fe")) != nil {
		t.Fatal("matched a suffix")
	}
}

func TestTaskForRelatedDirs(t *testing.T) {
	path, db := Fixture(t)
	root := t.TempDir()
	mustExec(t, db, `INSERT INTO tasks (id, project_id, title, status, directory, created_at, updated_at)
		VALUES ('proj', 'p', 'at the root', 'active', ?, 1, 10),
		       ('sub', 'p', 'in sub', 'active', ?, 2, 5),
		       ('root', 'p', 'filesystem root', 'active', '/', 3, 99)`, root, root+"/sub/")
	s := &Store{Path: path}
	defer s.Close()
	ctx := context.Background()
	for dir, want := range map[string]string{
		root + "/sub":      "sub",  // exact, despite the stored trailing slash
		root + "/sub/deep": "proj", // related tasks: live first, then newest
		root + "/other":    "proj", // below the project root
		root + "/":         "proj", // exact beats the newer subfolder task
		filepath.Dir(root): "proj", // above both; newest wins, "/" never does
	} {
		task, err := s.TaskFor(ctx, dir)
		if err != nil || task == nil || task.ID != want {
			t.Errorf("TaskFor(%s) = %+v, %v; want %s", dir, task, err, want)
		}
	}
	if task, _ := s.TaskFor(ctx, "/elsewhere"); task != nil {
		t.Errorf("unrelated dir matched %+v", task)
	}
}
