package bob

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

	task, err := taskFor(ctx, s, Proc{Dir: "/w"})
	if err != nil || task == nil || task.ID != "live" || !task.Live {
		t.Fatalf("TaskFor /w = %+v, %v; want the live task over the more recently updated one", task, err)
	}
	if task, _ := taskFor(ctx, s, Proc{Dir: "/nowhere"}); task != nil {
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
	if _, err := taskFor(context.Background(), s, Proc{Dir: "/w"}); err != ErrNoDB {
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
	// bob's system prompt isn't part of the conversation.
	if sys := Convert([]Row{{Seq: 4, Role: "system", Data: `{"role":"system","content":"<role_definition>You are Bob"}`}}); len(sys) != 0 {
		t.Errorf("system message shown: %+v", sys)
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
	out := []byte(`  101 ttys012  Thu Oct  8 13:58:01 2026     -zsh
  202 ttys012  Thu Oct  8 13:58:02 2026     node /opt/homebrew/bin/bob --auto-approve -r c388
  203 ttys012  Thu Oct  8 13:58:02 2026     /opt/homebrew/Cellar/node/22.1/bin/node --max-old-space-size=8192 /opt/homebrew/lib/node_modules/bobshell/dist/bob.js chat
  303 s013     Thu Oct  8 09:01:00 2026     bob --resume=abcd
  404 ??       Wed Oct  7 23:59:59 2026     node /x/bob.js
  505 ttys014  Thu Oct  8 13:58:01 2026     python3 scripts/bob-status.py --interval 1
  606 pts/3    Thu Oct  8 13:58:01 2026     vim bob.txt
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
	if got[0].Resume != "c388" || got[2].Resume != "abcd" || got[1].Resume != "" {
		t.Errorf("resume ids: %q %q %q", got[0].Resume, got[1].Resume, got[2].Resume)
	}
	if want := time.Date(2026, 10, 8, 13, 58, 2, 0, time.Local); !got[0].Start.Equal(want) {
		t.Errorf("start = %v, want %v", got[0].Start, want)
	}
}

func TestProcDetectorMatching(t *testing.T) {
	d := &ProcDetector{TTL: time.Hour, at: time.Now(), procs: []Proc{
		{PID: "1", TTY: "", Dir: "/w/env", Surface: "AB57-SURFACE-0000-0000-000000000000"},
		{PID: "2", TTY: "s013", Dir: "/w/tty"},
		{PID: "3", TTY: "ttys020", Dir: ""}, // cwd unknown: ignored
	}}
	ctx := context.Background()
	if p, ok := d.Lookup(ctx, "ab57-surface-0000-0000-000000000000", ""); !ok || p.Dir != "/w/env" {
		t.Errorf("by surface env: %q %v", p.Dir, ok)
	}
	if p, ok := d.Lookup(ctx, "other", "/dev/ttys013"); !ok || p.Dir != "/w/tty" {
		t.Errorf("by tty (short ps form): %q %v", p.Dir, ok)
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
		task, err := taskFor(ctx, s, Proc{Dir: dir})
		if err != nil || task == nil || task.ID != want {
			t.Errorf("TaskFor(%s) = %+v, %v; want %s", dir, task, err, want)
		}
	}
	if task, _ := taskFor(ctx, s, Proc{Dir: "/elsewhere"}); task != nil {
		t.Errorf("unrelated dir matched %+v", task)
	}
}

// taskFor assigns a session to a single bob process.
func taskFor(ctx context.Context, s *Store, p Proc) (*Task, error) {
	m, err := s.Assign(ctx, map[string]Proc{"sf": p})
	return m["sf"], err
}

// bob 2.0.5 leaves tasks.directory empty: sessions go to bobs by -r id,
// then by when each bob started, one terminal per session.
func TestAssignWithoutDirectories(t *testing.T) {
	path, db := Fixture(t)
	t0 := time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	ms := func(min int) int64 { return at(min).UnixMilli() }
	mustExec(t, db, `INSERT INTO tasks (id, project_id, title, status, directory, created_at, updated_at)
		VALUES ('old', 'p', 'old', 'active', '', ?, ?),
		       ('a1', 'p', 'a first', 'active', '', ?, ?),
		       ('resumed', 'p', 'resumed', 'active', '', ?, ?),
		       ('b1', 'p', 'b first', 'active', '', ?, ?)`,
		ms(-60), ms(-59), ms(1), ms(50), ms(-30), ms(55), ms(31), ms(32))
	s := &Store{Path: path}
	defer s.Close()
	got, err := s.Assign(context.Background(), map[string]Proc{
		"A": {Start: at(0)},                     // started 13:00, chatted at 13:01
		"B": {Start: at(30)},                    // started 13:30, chatted at 13:31
		"C": {Start: at(40)},                    // started 13:40, nothing yet
		"R": {Start: at(45), Resume: "resumed"}, // bob -r resumed
		"X": {Start: at(46), Resume: "missing"}, // -r of a task that's gone: by time
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for k, v := range got {
		ids[k] = v.ID
	}
	want := map[string]string{"A": "a1", "B": "b1", "R": "resumed"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Errorf("assigned %v, want %v", ids, want)
	}

	// A lone bob that started a new session (/new) moves on to it.
	mustExec(t, db, `INSERT INTO tasks (id, project_id, title, status, directory, created_at, updated_at) VALUES ('a2', 'p', 'a second', 'active', '', ?, ?)`, ms(60), ms(60))
	if task, _ := taskFor(context.Background(), s, Proc{Start: at(0)}); task == nil || task.ID != "a2" {
		t.Errorf("after /new: %+v", task)
	}
	if task, _ := taskFor(context.Background(), s, Proc{}); task != nil {
		t.Errorf("no start time matched %+v", task)
	}
}
