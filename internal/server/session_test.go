package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/savvychez/ray/internal/cmux"
)

func apply(prev []string, drop, keep int, add []string) []string {
	return append(append([]string{}, prev[drop:drop+keep]...), add...)
}

func TestDiffLines(t *testing.T) {
	seq := func(from, to int) []string {
		var s []string
		for i := from; i < to; i++ {
			s = append(s, fmt.Sprint("line ", i))
		}
		return s
	}
	tests := []struct {
		name       string
		prev, next []string
		maxSent    int // most lines we're willing to resend
	}{
		{"first", nil, seq(0, 10), 10},
		{"append", seq(0, 10), seq(0, 12), 2},
		{"scroll window", seq(0, 100), seq(5, 105), 5},
		{"redraw last line", append(seq(0, 10), "$ ec"), append(seq(0, 10), "$ echo"), 1},
		{"clear", seq(0, 50), []string{"$"}, 1},
		{"same", seq(0, 5), seq(0, 5), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drop, keep := diffLines(tt.prev, tt.next)
			sent := tt.next[keep:]
			if len(sent) > tt.maxSent {
				t.Errorf("sent %d lines, want <= %d (drop=%d keep=%d)", len(sent), tt.maxSent, drop, keep)
			}
			prev := tt.prev
			if prev == nil {
				prev = []string{}
			}
			if got := apply(prev, drop, keep, sent); !reflect.DeepEqual(got, tt.next) && !(len(got) == 0 && len(tt.next) == 0) {
				t.Errorf("apply = %q, want %q", got, tt.next)
			}
		})
	}
}

func TestBase32Code(t *testing.T) {
	p := &Pairing{}
	a, b := p.NewCode(0), p.NewCode(0)
	if a == b || len(a) != 26 {
		t.Fatalf("codes %q %q", a, b)
	}
}

// flakyBackend wraps FakeBackend; ReadText fails while failReads > 0.
type flakyBackend struct {
	*FakeBackend
	mu        sync.Mutex
	failReads int
	reads     int
}

func (f *flakyBackend) ReadText(ctx context.Context, ws, sf string, lines int) (string, error) {
	f.mu.Lock()
	f.reads++
	fail := f.failReads > 0
	if fail {
		f.failReads--
	}
	f.mu.Unlock()
	if fail {
		return "", fmt.Errorf("surface.read_text: %w", &cmux.RPCError{Code: "internal_error", Message: "Failed to read terminal text"})
	}
	return f.FakeBackend.ReadText(ctx, ws, sf, lines)
}

func (f *flakyBackend) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// pipeSession serves one session over an in-memory pipe and returns a
// channel of frames from the server.
func pipeSession(t *testing.T, b Backend, opts ...func(*Server)) (send func(Msg), frames <-chan Msg) {
	t.Helper()
	srvConn, cli := net.Pipe()
	srv := &Server{Backend: b, Logf: t.Logf, ScreenFast: 20 * time.Millisecond, ScreenSlow: 20 * time.Millisecond, TreeEvery: time.Hour}
	for _, o := range opts {
		o(srv)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(srvConn)
	}()
	// Wait for the session to exit so it can't log after the test ends.
	t.Cleanup(func() {
		cli.Close()
		<-done
	})
	ch := make(chan Msg, 256)
	go func() {
		rd := bufio.NewReader(cli)
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil {
				close(ch)
				return
			}
			var m Msg
			json.Unmarshal(line, &m)
			ch <- m
		}
	}()
	enc := json.NewEncoder(cli)
	send = func(m Msg) {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	send(Msg{T: "hello", Name: "test"})
	return send, ch
}

func TestTransientReadErrorsAreQuiet(t *testing.T) {
	fb := NewFakeBackend()
	tree, _ := fb.Tree(context.Background())
	ws, sf := tree[0].ID, tree[0].Surfaces[0].ID

	for _, tc := range []struct {
		fails      int
		wantNotice bool
	}{{2, false}, {10, true}} {
		t.Run(fmt.Sprint("fails=", tc.fails), func(t *testing.T) {
			b := &flakyBackend{FakeBackend: fb, failReads: tc.fails}
			send, frames := pipeSession(t, b)
			send(Msg{T: "watch", WS: ws, SF: sf})
			var gotNotice, gotScreen, gotClear bool
			timeout := time.After(5 * time.Second)
			for !gotScreen {
				select {
				case m := <-frames:
					switch {
					case m.T == "notice" && m.Err != "":
						gotNotice = true
					case m.T == "notice":
						gotClear = true
					case m.T == "screen":
						gotScreen = true
					}
				case <-timeout:
					t.Fatal("no screen frame")
				}
			}
			if gotNotice != tc.wantNotice {
				t.Errorf("notice shown = %v, want %v", gotNotice, tc.wantNotice)
			}
			if gotNotice && !gotClear {
				t.Error("notice was never cleared after recovery")
			}
		})
	}
}

func TestWatchNothingStopsStreaming(t *testing.T) {
	fb := NewFakeBackend()
	tree, _ := fb.Tree(context.Background())
	b := &flakyBackend{FakeBackend: fb}
	send, frames := pipeSession(t, b)
	send(Msg{T: "watch", WS: tree[0].ID, SF: tree[0].Surfaces[0].ID})
	for m := range frames {
		if m.T == "screen" {
			break
		}
	}
	// Wait for the server to have handled the stop (its ack), rather than
	// guessing with a sleep.
	send(Msg{T: "watch", ID: 1})
	for m := range frames {
		if m.T == "ack" && m.ID == 1 {
			break
		}
	}
	time.Sleep(50 * time.Millisecond) // let a read already in flight finish
	n := b.readCount()
	time.Sleep(200 * time.Millisecond)
	if d := b.readCount() - n; d > 0 {
		t.Fatalf("still reading after stop: %d more reads", d)
	}
}
