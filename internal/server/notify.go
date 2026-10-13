package server

import (
	"context"
	"strings"
	"time"

	"github.com/savvychez/ray/internal/bob"
	"github.com/savvychez/ray/internal/push"
)

// Pusher delivers notifications to devices that asked for them.
type Pusher interface {
	PublicKey() (string, error)
	Subscribe(device, name string, sub []byte) error
	Unsubscribe(device string) error
	// Send pushes to every subscribed device skip doesn't exclude and
	// returns how many deliveries were accepted.
	Send(ctx context.Context, n push.Notification, skip func(device string) bool) (int, error)
}

// bobSeen is what NotifyBob remembers about one bob terminal.
type bobSeen struct {
	task       string
	state      bob.State
	working    bool // seen working since the last "done"
	idlePolls  int  // consecutive polls idle
	approval   string
	approvalOK bool // approval already notified
}

// notifyIdlePolls is how many polls in a row bob must be idle before it
// counts as done: between tool calls it can look idle for a moment.
const notifyIdlePolls = 2

// NotifyBob watches every terminal running bob, whether or not a phone is
// connected, and pushes a notification when bob finishes a turn or asks
// for permission. A device that has the terminal open right now isn't
// notified about it.
func (s *Server) NotifyBob(ctx context.Context, every time.Duration) {
	if s.Bob == nil || s.Push == nil {
		return
	}
	seen := map[string]*bobSeen{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.notifyPoll(cctx, seen)
		cancel()
	}
}

func (s *Server) notifyPoll(ctx context.Context, seen map[string]*bobSeen) {
	tree, err := s.Backend.Tree(ctx)
	if err != nil {
		return
	}
	s.Bob.Annotate(ctx, tree)
	live := map[string]bool{}
	for _, ws := range tree {
		for _, sf := range ws.Surfaces {
			if !sf.Bob {
				continue
			}
			live[sf.ID] = true
			if n := s.bobEvent(ctx, ws, sf, seen); n != nil {
				sent, err := s.Push.Send(ctx, *n, func(dev string) bool { return s.watching(dev, sf.ID) })
				if err != nil {
					s.logf("push: %v", err)
				} else if sent > 0 {
					s.logf("push: %q to %d device(s)", n.Title, sent)
				}
			}
		}
	}
	for sf := range seen {
		if !live[sf] {
			delete(seen, sf)
		}
	}
}

// bobEvent updates what's known about bob terminal sf and returns the
// notification it calls for, if any.
func (s *Server) bobEvent(ctx context.Context, ws Workspace, sf Surface, seen map[string]*bobSeen) *push.Notification {
	task, ok, err := s.Bob.task(ctx, sf.ID)
	if !ok || err != nil || task == nil {
		return nil
	}
	text, err := s.Backend.ReadText(ctx, ws.ID, sf.ID, 0)
	if err != nil {
		return nil
	}
	s.Bob.noteScreen(sf.ID, text)
	prompt := bob.ParseScreen(text)
	rows, err := s.Bob.Store.Messages(ctx, task.ID, 0, 10)
	if err != nil {
		return nil
	}
	msgs := bob.Convert(rows)
	apps, _ := s.Bob.Store.Approvals(ctx, task.ID)
	live := *task
	live.Live = true // the process is running: that's how sf was matched
	state := bob.StateOf(&live, msgs, apps)
	approvalKey := ""
	switch {
	case prompt != nil:
		state, approvalKey = bob.StateApproval, prompt.Key()
	case len(apps) > 0:
		approvalKey = apps[0].RequestID
	}

	b := seen[sf.ID]
	if b == nil || b.task != task.ID {
		// First sight (or bob moved to another session): learn the state
		// without notifying about it.
		seen[sf.ID] = &bobSeen{task: task.ID, state: state, working: state != bob.StateIdle,
			approval: approvalKey, approvalOK: approvalKey != ""}
		return nil
	}
	b.state = state
	where := ws.Title
	if where == "" {
		where = sf.Title
	}
	n := &push.Notification{Tag: "bob-" + sf.ID, WS: ws.ID, SF: sf.ID}
	switch state {
	case bob.StateApproval:
		b.working, b.idlePolls = true, 0 // after approving, bob works again
		if approvalKey == b.approval && b.approvalOK {
			return nil
		}
		b.approval, b.approvalOK = approvalKey, true
		n.Title = "bob needs approval · " + where
		n.Body = approvalText(prompt, pendingCall(msgs))
		return n
	case bob.StateWorking:
		b.working, b.idlePolls, b.approval, b.approvalOK = true, 0, "", false
		return nil
	case bob.StateIdle:
		b.approval, b.approvalOK = "", false
		b.idlePolls++
		if !b.working || b.idlePolls < notifyIdlePolls {
			return nil
		}
		b.working = false
		n.Title = "bob is done · " + where
		n.Body = lastAnswer(msgs)
		if n.Body == "" {
			n.Body = task.Title
		}
		return n
	}
	return nil
}

func approvalText(p *bob.Prompt, call *bob.Call) string {
	var parts []string
	if p != nil {
		if p.Title != "" {
			parts = append(parts, p.Title)
		}
		if len(p.Detail) > 0 {
			parts = append(parts, p.Detail[0])
		}
	} else if call != nil {
		parts = append(parts, call.Name)
	}
	return clip(strings.Join(parts, ": "), 180)
}

// lastAnswer is the start of bob's latest reply.
func lastAnswer(msgs []bob.Msg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
			return clip(strings.Join(strings.Fields(m.Text), " "), 180)
		}
	}
	return ""
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
