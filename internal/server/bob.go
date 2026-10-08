package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/savvychez/ray/internal/bob"
)

// BobWatcher links cmux terminals running IBM Bob Shell to bob's session
// store, so the phone can show those terminals as a chat.
type BobWatcher struct {
	Store  *bob.Store
	Detect bob.Detector

	mu   sync.Mutex
	dirs map[string]string // surface id → directory bob runs in
}

// Annotate marks surfaces whose terminal is running bob (Surface.Bob).
func (w *BobWatcher) Annotate(ctx context.Context, wss []Workspace) {
	dirs := map[string]string{}
	for i := range wss {
		for j := range wss[i].Surfaces {
			s := &wss[i].Surfaces[j]
			if s.Type != "" && s.Type != "terminal" {
				continue
			}
			if dir, ok := w.Detect.Lookup(ctx, s.ID, s.TTY); ok {
				s.Bob = true
				dirs[s.ID] = dir
			}
		}
	}
	w.mu.Lock()
	w.dirs = dirs
	w.mu.Unlock()
}

func (w *BobWatcher) dir(surfaceID string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.dirs[surfaceID]
	return d, ok
}

// ChatFrame is the payload of a "chat" message: a bob session as chat.
type ChatFrame struct {
	Reset    bool          `json:"reset,omitempty"` // replace, don't append
	Task     *bob.Task     `json:"task,omitempty"`
	State    bob.State     `json:"state,omitempty"`
	Msgs     []bob.Msg     `json:"msgs,omitempty"`
	Approval *ChatApproval `json:"approval,omitempty"`
	Err      string        `json:"error,omitempty"`
}

// ChatApproval is a pending permission prompt, with the tool call it is
// most likely about (bob's newest call that has no result yet).
type ChatApproval struct {
	RequestID string          `json:"request_id"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Call      *bob.Call       `json:"call,omitempty"`
}

// chatState is what a session remembers about the bob chat it streams.
type chatState struct {
	surface string
	taskID  string
	seq     int64
	tail    []bob.Msg // recent messages, for state and pending-call detection
	sig     string
	errSent string
	last    time.Time
}

const (
	chatInitial = 150 // messages sent when a chat opens
	chatTail    = 40
)

// poll sends the next chat frame for surface sf, if anything changed.
func (w *BobWatcher) poll(ctx context.Context, ws, sf string, cs *chatState, send func(Msg) bool) {
	if cs.surface != sf {
		*cs = chatState{surface: sf}
	}
	dir, ok := w.dir(sf)
	if !ok {
		return // not a bob terminal (or no longer)
	}
	fail := func(err error) {
		msg := err.Error()
		if errors.Is(err, bob.ErrNoDB) {
			msg = "bob is running here, but its session store (" + w.Store.Path + ") wasn't found"
		}
		if msg != cs.errSent {
			cs.errSent = msg
			send(Msg{T: "chat", WS: ws, SF: sf, Chat: &ChatFrame{Err: msg}})
		}
	}
	task, err := w.Store.TaskFor(ctx, dir)
	if err != nil {
		fail(err)
		return
	}
	if task == nil {
		fail(fmt.Errorf("no bob session found for %s yet", dir))
		return
	}
	cs.errSent = ""

	reset := task.ID != cs.taskID
	after, limit := cs.seq, 500
	if reset {
		after, limit = 0, chatInitial
		cs.tail = nil
	}
	rows, err := w.Store.Messages(ctx, task.ID, after, limit)
	if err != nil {
		fail(err)
		return
	}
	msgs := bob.Convert(rows)
	if len(rows) > 0 {
		cs.seq = rows[len(rows)-1].Seq
	}
	cs.taskID = task.ID
	cs.tail = append(cs.tail, msgs...)
	if len(cs.tail) > chatTail {
		cs.tail = cs.tail[len(cs.tail)-chatTail:]
	}

	apps, err := w.Store.Approvals(ctx, task.ID)
	if err != nil {
		fail(err)
		return
	}
	f := &ChatFrame{Reset: reset, Task: task, State: bob.StateOf(task, cs.tail, apps), Msgs: msgs}
	if len(apps) > 0 {
		a := apps[0]
		f.Approval = &ChatApproval{RequestID: a.RequestID, Payload: bob.TrimPayload(a.Payload), Call: pendingCall(cs.tail)}
	}
	sig := fmt.Sprint(f.State, task.Title, task.Cost, task.Live, approvalID(f.Approval))
	if !reset && len(msgs) == 0 && sig == cs.sig {
		return
	}
	if send(Msg{T: "chat", WS: ws, SF: sf, Chat: f}) {
		cs.sig = sig
	} else {
		cs.taskID = "" // dropped: resend everything next time
	}
}

func approvalID(a *ChatApproval) string {
	if a == nil {
		return ""
	}
	return a.RequestID
}

// pendingCall is the newest tool call without a result.
func pendingCall(msgs []bob.Msg) *bob.Call {
	done := map[string]bool{}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Res != nil {
			done[m.Res.CallID] = true
		}
		for j := len(m.Calls) - 1; j >= 0; j-- {
			if c := m.Calls[j]; !done[c.ID] {
				return &c
			}
		}
	}
	return nil
}

// approvalKeys are the keystrokes for bob's approval menu, which always
// opens on "Approve Once", with "Reject" third:
//
//	→ Approve Once
//	  Approve … for task / Always Allow … for task
//	  Reject
var approvalKeys = map[string][]string{
	"once":   {"enter"},
	"always": {"down", "enter"},
	"reject": {"down", "down", "enter"},
}

// approve answers bob's pending permission prompt in terminal sf.
func (w *BobWatcher) approve(ctx context.Context, b Backend, ws, sf, choice string) error {
	keys, ok := approvalKeys[choice]
	if !ok {
		return fmt.Errorf("unknown choice %q", choice)
	}
	dir, ok := w.dir(sf)
	if !ok {
		return errors.New("this terminal isn't running bob")
	}
	task, err := w.Store.TaskFor(ctx, dir)
	if err != nil || task == nil {
		return fmt.Errorf("no bob session: %v", err)
	}
	// Only press keys if bob is really waiting; otherwise they'd land in
	// whatever bob is showing now.
	apps, err := w.Store.Approvals(ctx, task.ID)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		return errors.New("bob isn't waiting for an approval anymore")
	}
	for i, k := range keys {
		if i > 0 {
			time.Sleep(60 * time.Millisecond) // let the TUI redraw between keys
		}
		if err := b.SendKey(ctx, ws, sf, k); err != nil {
			return err
		}
	}
	return nil
}
