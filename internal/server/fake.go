package server

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// FakeBackend is an in-memory stand-in for cmux, for trying the PWA on a
// machine without cmux and for tests. Each surface is a tiny line-mode
// shell: typed text is buffered and `enter` runs it with sh -c.
type FakeBackend struct {
	mu         sync.Mutex
	workspaces []*fakeWS
	n          int

	// OnInput, if set, sees every text and key sent to a surface (kind is
	// "text" or "key"); tests and the demo use it to simulate programs.
	OnInput func(surfaceID, kind, value string)
}

type fakeWS struct {
	Workspace
	terms map[string]*fakeTerm
}

type fakeTerm struct {
	lines []string
	input string
}

func NewFakeBackend() *FakeBackend {
	f := &FakeBackend{}
	f.addWorkspace("~/ray")
	f.workspaces[0].Selected = true
	f.addSurface(f.workspaces[0], "logs")
	// A sidebar group, like cmux's: the header is itself a workspace (the
	// anchor) with its own terminal, followed by its members.
	agents := f.addWorkspace("agents")
	claude := f.addWorkspace("claude")
	g := &GroupRef{ID: "grp-1", Name: "agents"}
	agents.Group = &GroupRef{ID: g.ID, Name: g.Name, Anchor: true}
	claude.Group = g
	f.addWorkspace("scratch")
	return f
}

func (f *FakeBackend) Name() string { return "fake" }

func (f *FakeBackend) addWorkspace(title string) *fakeWS {
	f.n++
	ws := &fakeWS{Workspace: Workspace{ID: fmt.Sprintf("ws-%d", f.n), Index: len(f.workspaces), Title: title}, terms: map[string]*fakeTerm{}}
	f.workspaces = append(f.workspaces, ws)
	f.addSurface(ws, "zsh")
	return ws
}

func (f *FakeBackend) addSurface(ws *fakeWS, title string) {
	f.n++
	id := fmt.Sprintf("sf-%d", f.n)
	for i := range ws.Surfaces {
		ws.Surfaces[i].Focused = false
	}
	ws.Surfaces = append(ws.Surfaces, Surface{ID: id, Index: len(ws.Surfaces), Title: title, Type: "terminal", Focused: true, TTY: "fake-" + id})
	ws.terms[id] = &fakeTerm{lines: []string{
		"ray demo terminal (" + ws.Title + " / " + title + ")",
		"commands typed here run with sh -c on the server",
		"$ ",
	}}
}

func (f *FakeBackend) Tree(ctx context.Context) ([]Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Workspace, 0, len(f.workspaces))
	for _, ws := range f.workspaces {
		w := ws.Workspace
		w.Surfaces = append([]Surface(nil), ws.Surfaces...)
		out = append(out, w)
	}
	return out, nil
}

func (f *FakeBackend) term(wsID, sfID string) (*fakeWS, *fakeTerm, error) {
	for _, ws := range f.workspaces {
		if wsID != "" && ws.ID != wsID {
			continue
		}
		if wsID == "" && !ws.Selected {
			continue
		}
		if sfID == "" {
			for _, s := range ws.Surfaces {
				if s.Focused {
					sfID = s.ID
				}
			}
		}
		if t, ok := ws.terms[sfID]; ok {
			return ws, t, nil
		}
	}
	return nil, nil, fmt.Errorf("not_found: surface %q", sfID)
}

func (f *FakeBackend) ReadText(ctx context.Context, wsID, sfID string, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, t, err := f.term(wsID, sfID)
	if err != nil {
		return "", err
	}
	ls := append([]string(nil), t.lines...)
	ls[len(ls)-1] += t.input
	if lines > 0 && len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return strings.Join(ls, "\n"), nil
}

func (f *FakeBackend) SendText(ctx context.Context, wsID, sfID, text string) error {
	if f.OnInput != nil {
		f.OnInput(sfID, "text", text)
	}
	for _, r := range text {
		var err error
		switch r {
		case '\r', '\n':
			err = f.SendKey(ctx, wsID, sfID, "enter")
		case 3:
			err = f.SendKey(ctx, wsID, sfID, "ctrl-c")
		case 0x7f, 8:
			err = f.SendKey(ctx, wsID, sfID, "backspace")
		default:
			f.mu.Lock()
			var t *fakeTerm
			_, t, err = f.term(wsID, sfID)
			if err == nil && r >= 0x20 {
				t.input += string(r)
			}
			f.mu.Unlock()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *FakeBackend) SendKey(ctx context.Context, wsID, sfID, key string) error {
	if f.OnInput != nil {
		f.OnInput(sfID, "key", key)
	}
	f.mu.Lock()
	_, t, err := f.term(wsID, sfID)
	if err != nil {
		f.mu.Unlock()
		return err
	}
	switch key {
	case "enter":
		cmd := t.input
		t.lines[len(t.lines)-1] += cmd
		t.input = ""
		f.mu.Unlock()
		out := runFake(cmd)
		f.mu.Lock()
		t.lines = append(t.lines, out...)
		t.lines = append(t.lines, "$ ")
		if len(t.lines) > 2000 {
			t.lines = t.lines[len(t.lines)-2000:]
		}
	case "backspace":
		if r := []rune(t.input); len(r) > 0 {
			t.input = string(r[:len(r)-1])
		}
	case "ctrl-c":
		t.lines[len(t.lines)-1] += t.input + "^C"
		t.input = ""
		t.lines = append(t.lines, "$ ")
	}
	f.mu.Unlock()
	return nil
}

func runFake(cmd string) []string {
	if strings.TrimSpace(cmd) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	s := strings.TrimRight(string(out), "\n")
	var lines []string
	if s != "" {
		lines = strings.Split(s, "\n")
	}
	if err != nil {
		lines = append(lines, "["+err.Error()+"]")
	}
	return lines
}

func (f *FakeBackend) NewWorkspace(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws := f.addWorkspace(fmt.Sprintf("workspace %d", len(f.workspaces)+1))
	return ws.ID, nil
}

func (f *FakeBackend) Focus(ctx context.Context, wsID, sfID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ws := range f.workspaces {
		ws.Selected = ws.ID == wsID
		if ws.Selected && sfID != "" {
			for i := range ws.Surfaces {
				ws.Surfaces[i].Focused = ws.Surfaces[i].ID == sfID
			}
		}
	}
	return nil
}
