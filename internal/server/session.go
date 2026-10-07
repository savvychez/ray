package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/savvychez/ray/internal/cmux"
)

// ProtocolVersion is bumped on incompatible wire changes.
const ProtocolVersion = 1

// Msg is one newline-delimited JSON frame in either direction. Only the
// fields relevant to a given type "t" are set.
//
// Client → server:
//
//	hello  {name, pair?}           first frame; pair is a one-time pairing code
//	watch  {ws, sf, lines?}        stream this surface's text
//	text   {ws, sf, data}          type text into a surface
//	key    {ws, sf, key}           press a named key (enter, tab, escape, up, …)
//	focus  {ws, sf}                bring a workspace/surface to front on the Mac
//	new    {}                      create a workspace
//	ping   {}
//
// Server → client:
//
//	welcome {host, backend, version}
//	tree    {workspaces}           sent on connect and whenever it changes
//	screen  {ws, sf, drop, keep, lines}
//	                               drop the first `drop` lines, keep the next
//	                               `keep`, discard the rest, append `lines`
//	ack     {id, error?}           reply to any client frame that carried an id
//	error   {code, error}          fatal; the server closes the stream after it
//	pong    {}
type Msg struct {
	T   string `json:"t"`
	ID  int    `json:"id,omitempty"`
	Err string `json:"error,omitempty"`

	// hello / welcome
	V       int    `json:"v,omitempty"`
	Name    string `json:"name,omitempty"`
	Pair    string `json:"pair,omitempty"`
	Host    string `json:"host,omitempty"`
	Backend string `json:"backend,omitempty"`
	Version string `json:"version,omitempty"`
	Code    string `json:"code,omitempty"`

	// targeting
	WS string `json:"ws,omitempty"`
	SF string `json:"sf,omitempty"`

	Data       string      `json:"data,omitempty"`
	Key        string      `json:"key,omitempty"`
	Lines      []string    `json:"lines,omitempty"`
	NumLines   int         `json:"n,omitempty"`
	Drop       int         `json:"drop,omitempty"`
	Keep       *int        `json:"keep,omitempty"`
	Workspaces []Workspace `json:"workspaces,omitempty"`
}

// Authorizer decides whether a connecting peer may use the server.
type Authorizer interface {
	// Authorize is called with the hello frame. It returns nil to admit.
	Authorize(conn net.Conn, hello Msg) error
}

// Server serves sessions on connections handed to it by a transport.
type Server struct {
	Backend  Backend
	Auth     Authorizer
	Hostname string
	Version  string // ray build, reported to clients
	Logf     func(format string, args ...any)

	// Poll intervals; zero values use defaults.
	TreeEvery   time.Duration
	ScreenFast  time.Duration
	ScreenSlow  time.Duration
	DefaultTail int
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// ErrUnauthorized is returned by authorizers to reject a peer.
var ErrUnauthorized = errors.New("unauthorized")

// Serve runs one session until the connection closes.
func (s *Server) Serve(conn net.Conn) {
	defer conn.Close()
	ss := &session{
		srv:   s,
		conn:  conn,
		out:   make(chan Msg, 64),
		watch: make(chan watchReq, 1),
		poke:  make(chan struct{}, 1),
	}
	ss.run()
}

type watchReq struct {
	ws, sf string
	lines  int
}

type session struct {
	srv  *Server
	conn net.Conn
	out  chan Msg

	watch chan watchReq
	poke  chan struct{} // input happened; refresh screen and tree soon

	mu     sync.Mutex
	active time.Time // last input
}

func (ss *session) send(m Msg) bool {
	select {
	case ss.out <- m:
		return true
	default:
		// Slow client: drop rather than block the pollers. screen frames
		// are full-state-recoverable because the next poll resends.
		return false
	}
}

func (ss *session) run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rd := bufio.NewReaderSize(ss.conn, 64<<10)
	ss.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	hello, err := readMsg(rd)
	if err != nil {
		ss.srv.logf("session %v: reading hello: %v", ss.conn.RemoteAddr(), err)
		return
	}
	ss.conn.SetReadDeadline(time.Time{})
	enc := json.NewEncoder(ss.conn)
	if hello.T != "hello" {
		enc.Encode(Msg{T: "error", Code: "protocol", Err: "expected hello"})
		return
	}
	if ss.srv.Auth != nil {
		if err := ss.srv.Auth.Authorize(ss.conn, hello); err != nil {
			ss.srv.logf("session %v (%q): rejected: %v", ss.conn.RemoteAddr(), hello.Name, err)
			enc.Encode(Msg{T: "error", Code: "unauthorized", Err: err.Error()})
			return
		}
	}
	ss.srv.logf("session %v (%q): connected", ss.conn.RemoteAddr(), hello.Name)
	defer ss.srv.logf("session %v (%q): disconnected", ss.conn.RemoteAddr(), hello.Name)

	if err := enc.Encode(Msg{T: "welcome", V: ProtocolVersion, Host: ss.srv.Hostname, Backend: ss.srv.Backend.Name(), Version: ss.srv.Version}); err != nil {
		return
	}

	// Writer.
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-ss.out:
				if err := enc.Encode(m); err != nil {
					return
				}
			}
		}
	}()
	go ss.pollTree(ctx)
	go ss.pollScreen(ctx)

	// Reader.
	for {
		m, err := readMsg(rd)
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				ss.srv.logf("session %v: read: %v", ss.conn.RemoteAddr(), err)
			}
			return
		}
		ss.handle(ctx, m)
		if ctx.Err() != nil {
			return
		}
	}
}

func readMsg(rd *bufio.Reader) (Msg, error) {
	var m Msg
	line, err := rd.ReadBytes('\n')
	if err != nil {
		if len(line) == 0 {
			return m, err
		}
	}
	if len(line) > 1<<20 {
		return m, errors.New("frame too large")
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return m, err
	}
	return m, nil
}

func (ss *session) markActive() {
	ss.mu.Lock()
	ss.active = time.Now()
	ss.mu.Unlock()
	select {
	case ss.poke <- struct{}{}:
	default:
	}
}

func (ss *session) recentlyActive() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return time.Since(ss.active) < 15*time.Second
}

func (ss *session) handle(ctx context.Context, m Msg) {
	b := ss.srv.Backend
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var err error
	switch m.T {
	case "ping":
		ss.send(Msg{T: "pong", ID: m.ID})
		return
	case "watch":
		lines := m.NumLines
		if lines <= 0 {
			lines = ss.srv.DefaultTail
		}
		// Replace any pending watch request.
		select {
		case <-ss.watch:
		default:
		}
		ss.watch <- watchReq{ws: m.WS, sf: m.SF, lines: lines}
		ss.markActive()
	case "text":
		err = b.SendText(cctx, m.WS, m.SF, m.Data)
		ss.markActive()
	case "key":
		err = b.SendKey(cctx, m.WS, m.SF, m.Key)
		ss.markActive()
	case "focus":
		err = b.Focus(cctx, m.WS, m.SF)
		ss.markActive()
	case "new":
		_, err = b.NewWorkspace(cctx)
		ss.markActive()
	default:
		err = errors.New("unknown message type " + m.T)
	}
	if m.ID != 0 {
		r := Msg{T: "ack", ID: m.ID}
		if err != nil {
			r.Err = err.Error()
		}
		ss.send(r)
	} else if err != nil {
		ss.srv.logf("session %v: %s: %v", ss.conn.RemoteAddr(), m.T, err)
	}
}

func durOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (ss *session) pollTree(ctx context.Context) {
	every := durOr(ss.srv.TreeEvery, 2*time.Second)
	var last []Workspace
	var lastErr string
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		tree, err := ss.srv.Backend.Tree(cctx)
		cancel()
		if err != nil {
			if err.Error() != lastErr {
				lastErr = err.Error()
				ss.send(Msg{T: "notice", Err: "cmux: " + lastErr})
			}
		} else {
			lastErr = ""
			if tree == nil {
				tree = []Workspace{}
			}
			if !reflect.DeepEqual(tree, last) && ss.send(Msg{T: "tree", Workspaces: tree}) {
				last = tree
			}
		}
		t.Reset(every)
	}
}

func (ss *session) pollScreen(ctx context.Context) {
	fast := durOr(ss.srv.ScreenFast, 250*time.Millisecond)
	slow := durOr(ss.srv.ScreenSlow, 1200*time.Millisecond)
	var cur *watchReq
	var prev []string
	var lastErr string
	var fails int
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-ss.watch:
			cur, prev, lastErr, fails = &w, nil, "", 0
			if w.ws == "" && w.sf == "" {
				cur = nil // watch with no target stops streaming
			}
		case <-ss.poke:
			// Give the program a moment to react to the input.
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Millisecond):
			}
		case <-t.C:
		}
		if cur == nil {
			t.Reset(time.Hour)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		text, err := ss.srv.Backend.ReadText(cctx, cur.ws, cur.sf, cur.lines)
		cancel()
		if err != nil {
			fails++
			// cmux's read_text sometimes fails for a moment (internal_error
			// "Failed to read terminal text", e.g. mid-redraw). Keep showing
			// the last screen and only report it if it persists.
			if (!transientReadError(err) || fails >= 4) && err.Error() != lastErr {
				lastErr = err.Error()
				ss.send(Msg{T: "notice", WS: cur.ws, SF: cur.sf, Err: lastErr})
			}
		} else {
			fails = 0
			if lastErr != "" {
				lastErr = ""
				ss.send(Msg{T: "notice", WS: cur.ws, SF: cur.sf}) // clears it
			}
			lines := splitLines(text)
			if prev == nil || !equalLines(prev, lines) {
				drop, keep := diffLines(prev, lines)
				if ss.send(Msg{T: "screen", WS: cur.ws, SF: cur.sf, Drop: drop, Keep: &keep, Lines: lines[keep:]}) {
					prev = lines
				} else {
					prev = nil // force a full resend
				}
			}
		}
		if ss.recentlyActive() {
			t.Reset(fast)
		} else {
			t.Reset(slow)
		}
	}
}

// transientReadError reports whether a read failure is worth retrying
// quietly rather than showing right away.
func transientReadError(err error) bool {
	var rerr *cmux.RPCError
	if errors.As(err, &rerr) {
		return rerr.Code == "internal_error" || rerr.Code == "rate_limited"
	}
	return true // transport hiccup; the next poll redials
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return []string{}
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

func commonPrefix(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func equalLines(a, b []string) bool {
	return len(a) == len(b) && commonPrefix(a, b) == len(a)
}

// diffLines finds how to turn prev into next cheaply: drop the first
// `drop` lines of prev, keep the following `keep` lines, and append
// next[keep:]. It handles both in-place redraws (drop=0) and the
// scrolling window of a tailed terminal (drop>0).
func diffLines(prev, next []string) (drop, keep int) {
	if prev == nil {
		return 0, 0
	}
	keep = commonPrefix(prev, next)
	const maxShift = 400
	for d := 1; d < len(prev) && d <= maxShift; d++ {
		if len(prev)-d <= keep {
			break // can't beat the current best
		}
		if k := commonPrefix(prev[d:], next); k > keep {
			drop, keep = d, k
		}
	}
	return drop, keep
}
