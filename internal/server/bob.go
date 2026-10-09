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

	mu        sync.Mutex
	procs     map[string]bob.Proc // surface id → the bob running there
	assigned  map[string]string   // surface id → task id it shows
	screens   map[string]screenAt // latest text of watched bob terminals
	assignAt  time.Time
	assignErr error
}

// Annotate marks surfaces whose terminal is running bob (Surface.Bob).
func (w *BobWatcher) Annotate(ctx context.Context, wss []Workspace) {
	procs := map[string]bob.Proc{}
	for i := range wss {
		for j := range wss[i].Surfaces {
			s := &wss[i].Surfaces[j]
			if s.Type != "" && s.Type != "terminal" {
				continue
			}
			if p, ok := w.Detect.Lookup(ctx, s.ID, s.TTY); ok {
				s.Bob = true
				procs[s.ID] = p
			}
		}
	}
	w.mu.Lock()
	w.procs = procs
	w.assignAt = time.Time{} // re-assign sessions on next use
	w.mu.Unlock()
}

type screenAt struct {
	text string
	at   time.Time
}

// noteScreen records what bob terminal sf shows: evidence for which of
// its sessions bob is in.
func (w *BobWatcher) noteScreen(sf, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.procs[sf]; !ok {
		return
	}
	if w.screens == nil {
		w.screens = map[string]screenAt{}
	}
	w.screens[sf] = screenAt{text, time.Now()}
}

// assignEvery bounds how stale the terminal → session assignment gets; a
// new session (bob's first message) shows up within this.
const assignEvery = time.Second

// task finds the bob session running in terminal sf; ok is false if sf
// isn't running bob.
func (w *BobWatcher) task(ctx context.Context, sf string) (task *bob.Task, ok bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.procs[sf]; !ok {
		return nil, false, nil
	}
	if time.Since(w.assignAt) > assignEvery {
		screens := map[string]string{}
		for k, sc := range w.screens {
			if time.Since(sc.at) < 30*time.Second {
				screens[k] = sc.text
			}
		}
		got, err := w.Store.Assign(ctx, w.procs, bob.Hints{Screens: screens, Prev: w.assigned})
		w.assigned, w.assignErr, w.assignAt = map[string]string{}, err, time.Now()
		for k, t := range got {
			w.assigned[k] = t.ID
		}
	}
	if w.assignErr != nil {
		return nil, true, w.assignErr
	}
	id := w.assigned[sf]
	if id == "" {
		return nil, true, nil
	}
	task, err = w.Store.Task(ctx, id)
	return task, true, err
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
	Prompt    *bob.Prompt     `json:"prompt,omitempty"` // the menu as bob draws it
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
	prompt  *bob.Prompt // permission menu on screen, from the last read
}

const (
	chatInitial = 150 // messages sent when a chat opens
	chatTail    = 40
)

// screen notes the terminal's latest text, where bob draws its
// permission prompts.
func (cs *chatState) screen(sf, text string) {
	if cs.surface == sf {
		cs.prompt = bob.ParseScreen(text)
	}
}

// poll sends the next chat frame for surface sf, if anything changed.
func (w *BobWatcher) poll(ctx context.Context, ws, sf string, cs *chatState, send func(Msg) bool) {
	if cs.surface != sf {
		*cs = chatState{surface: sf}
	}
	task, ok, err := w.task(ctx, sf)
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
	if err != nil {
		fail(err)
		return
	}
	if task == nil {
		fail(errors.New("no bob session here yet; it appears once you send bob a message"))
		return
	}
	cs.errSent = ""
	// The process is running (that's how this terminal was matched), whatever
	// bob's session lease says; bob 2.0.5 doesn't keep one.
	t := *task
	t.Live = true
	task = &t

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
		f.Approval = &ChatApproval{RequestID: a.RequestID, Payload: bob.TrimPayload(a.Payload), Call: pendingCall(cs.tail), Prompt: cs.prompt}
	} else if cs.prompt != nil {
		f.State = bob.StateApproval
		f.Approval = &ChatApproval{RequestID: cs.prompt.Key(), Call: pendingCall(cs.tail), Prompt: cs.prompt}
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

// dbApproval reports whether bob's store has a pending approval for the
// session in terminal sf.
func (w *BobWatcher) dbApproval(ctx context.Context, sf string) bool {
	task, _, err := w.task(ctx, sf)
	if err != nil || task == nil {
		return false
	}
	apps, err := w.Store.Approvals(ctx, task.ID)
	return err == nil && len(apps) > 0
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

// approvalKeys are the keystrokes for bob's approval menu, which opens on
// "Approve Once", with "Reject" third:
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
	if _, ok, _ := w.task(ctx, sf); !ok {
		return errors.New("this terminal isn't running bob")
	}
	// Only press keys if bob is really waiting; otherwise they'd land in
	// whatever bob is showing now. The menu on screen is the surest sign,
	// and says where its cursor is (someone may have moved it at the
	// laptop); bob's pending-approvals table is the fallback.
	text, err := b.ReadText(ctx, ws, sf, 0)
	if p := bob.ParseScreen(text); err == nil && p != nil {
		target := p.Index(choice)
		if target < 0 {
			return fmt.Errorf("bob's prompt has no %q option", choice)
		}
		keys = nil
		for i := p.Cursor; i < target; i++ {
			keys = append(keys, "down")
		}
		for i := p.Cursor; i > target; i-- {
			keys = append(keys, "up")
		}
		keys = append(keys, "enter")
	} else if !w.dbApproval(ctx, sf) {
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
