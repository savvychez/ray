// Package cmux talks to a running cmux app through its v2 JSON-RPC API.
//
// Two transports are supported:
//
//   - the Unix socket (newline-delimited JSON, one request per line), which
//     works when cmux's socket access mode admits this process, and
//   - the bundled `cmux rpc <method> <json>` CLI, which works in cmux's default
//     "cmux processes only" mode no matter how ray itself was started.
//
// Dial tries the socket first and falls back to the CLI.
package cmux

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Caller makes a single v2 RPC and returns its raw "result".
type Caller interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Name() string
	Close() error
}

// RPCError is an error returned by cmux itself (ok:false).
type RPCError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// retryAfter reports how long cmux asked us to back off, for rate_limited
// replies.
func (e *RPCError) retryAfter() (time.Duration, bool) {
	if e.Code != "rate_limited" {
		return 0, false
	}
	var d struct {
		RetryAfterMS int `json:"retry_after_ms"`
	}
	json.Unmarshal(e.Data, &d)
	return time.Duration(max(d.RetryAfterMS, 20)) * time.Millisecond, true
}

// pollingMethods are the read methods cmux meters per connection with a
// token bucket (burst 9, one token per 100ms); see cmux's
// ControlClientRateLimiter. Everything else (input, focus, …) is unmetered.
var pollingMethods = map[string]bool{
	"system.top": true, "system.memory": true, "system.tree": true, "system.identify": true,
	"window.list": true, "window.current": true, "window.displays": true,
	"workspace.list": true, "workspace.current": true,
	"surface.list": true, "surface.current": true, "surface.read_text": true, "surface.read_selection": true,
	"pane.list": true, "pane.surfaces": true,
}

// bucket mirrors cmux's per-connection limiter on our side, so polls wait
// their turn instead of bouncing off rate_limited errors. It keeps one
// token in reserve.
type bucket struct {
	tokens float64
	last   time.Time
}

const (
	bucketBurst = 8
	bucketRate  = 10.0 // tokens per second
)

// wait blocks until a polling call may go out.
func (b *bucket) wait(ctx context.Context) error {
	for {
		now := time.Now()
		if b.last.IsZero() {
			b.tokens, b.last = bucketBurst, now
		}
		b.tokens = min(bucketBurst, b.tokens+now.Sub(b.last).Seconds()*bucketRate)
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			return nil
		}
		d := time.Duration((1 - b.tokens) / bucketRate * float64(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

// withRateLimitRetry retries a call that cmux rate-limited, honoring its
// retry_after_ms hint.
func withRateLimitRetry(ctx context.Context, call func() (json.RawMessage, error)) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		res, err := call()
		var rerr *RPCError
		if attempt >= 5 || !errors.As(err, &rerr) {
			return res, err
		}
		d, ok := rerr.retryAfter()
		if !ok {
			return res, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d):
		}
	}
}

func (e *RPCError) Error() string { return e.Code + ": " + e.Message }

// DefaultSocketPath returns $CMUX_SOCKET_PATH or the release default.
func DefaultSocketPath() string {
	if p := os.Getenv("CMUX_SOCKET_PATH"); p != "" {
		return p
	}
	return "/tmp/cmux.sock"
}

// FindCLI locates the cmux CLI binary.
func FindCLI() string {
	if p := os.Getenv("CMUX_BIN"); p != "" {
		return p
	}
	if p, err := exec.LookPath("cmux"); err == nil {
		return p
	}
	for _, p := range []string{
		"/Applications/cmux.app/Contents/Resources/bin/cmux",
		os.ExpandEnv("$HOME/Applications/cmux.app/Contents/Resources/bin/cmux"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Dial picks a transport. mode is "auto", "socket" or "cli".
func Dial(ctx context.Context, mode, socketPath, cliPath string) (Caller, error) {
	var errs []error
	if mode == "auto" || mode == "socket" {
		s := &SocketCaller{Path: socketPath}
		if _, err := s.Call(ctx, "system.ping", map[string]any{}); err == nil {
			return s, nil
		} else {
			s.Close()
			errs = append(errs, fmt.Errorf("socket %s: %w", socketPath, err))
		}
	}
	if mode == "auto" || mode == "cli" {
		if cliPath == "" {
			cliPath = FindCLI()
		}
		if cliPath == "" {
			errs = append(errs, errors.New("cmux CLI not found (set CMUX_BIN or put cmux on PATH)"))
		} else {
			c := &CLICaller{Path: cliPath}
			if _, err := c.Call(ctx, "system.ping", map[string]any{}); err == nil {
				return c, nil
			} else {
				errs = append(errs, fmt.Errorf("cli %s: %w", cliPath, err))
			}
		}
	}
	return nil, errors.Join(errs...)
}

// SocketCaller speaks to the cmux Unix socket over one persistent
// connection, redialing on failure. Calls are serialized.
type SocketCaller struct {
	Path string

	mu       sync.Mutex
	bucket   bucket
	conn     net.Conn
	lastUsed time.Time
	rd       *bufio.Reader
	next     int
}

func (s *SocketCaller) Name() string { return "socket " + s.Path }

func (s *SocketCaller) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

func (s *SocketCaller) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return withRateLimitRetry(ctx, func() (json.RawMessage, error) {
		if pollingMethods[method] {
			if err := s.bucket.wait(ctx); err != nil {
				return nil, err
			}
		}
		reused := s.conn != nil
		res, err := s.callLocked(ctx, method, params)
		if err == nil || isRPCError(err) {
			return res, err
		}
		s.dropLocked()
		// cmux closes connections that sit idle, so a reused one can be
		// dead. Redial and retry once when that can't duplicate an effect:
		// the request never went out, or it's a read-only poll.
		var we *writeError
		if reused && ctx.Err() == nil && (errors.As(err, &we) || pollingMethods[method]) {
			res, err = s.callLocked(ctx, method, params)
			if err != nil && !isRPCError(err) {
				s.dropLocked()
			}
		}
		return res, err
	})
}

func (s *SocketCaller) dropLocked() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// writeError marks a failure to send a request, which therefore never
// reached cmux.
type writeError struct{ err error }

func (e *writeError) Error() string { return e.err.Error() }
func (e *writeError) Unwrap() error { return e.err }

// maxIdle is how long a connection may sit unused before we redial instead
// of reusing it; cmux closes client connections after 30s without a request.
var maxIdle = 20 * time.Second

func (s *SocketCaller) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if s.conn != nil && time.Since(s.lastUsed) > maxIdle {
		s.dropLocked()
	}
	s.lastUsed = time.Now()
	if s.conn == nil {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", s.Path)
		if err != nil {
			return nil, err
		}
		s.conn = c
		s.rd = bufio.NewReaderSize(c, 1<<20)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(20 * time.Second)
	}
	s.conn.SetDeadline(deadline)
	s.next++
	id := s.next
	line, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err := s.conn.Write(append(line, '\n')); err != nil {
		return nil, &writeError{err}
	}
	for {
		b, err := s.rd.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp struct {
			ID     any             `json:"id"`
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			// v1 servers and access-denied replies are plain text.
			return nil, fmt.Errorf("unexpected reply: %q", truncate(string(b), 200))
		}
		if fmt.Sprint(resp.ID) != fmt.Sprint(id) {
			continue
		}
		if !resp.OK {
			if resp.Error == nil {
				resp.Error = &RPCError{Code: "error", Message: "unknown error"}
			}
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// CLICaller shells out to `cmux rpc`. Params go over stdin so terminal
// input never shows up in the process list.
type CLICaller struct {
	Path string
}

func (c *CLICaller) Name() string { return "cli " + c.Path }
func (c *CLICaller) Close() error { return nil }

func (c *CLICaller) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, c.Path, "rpc", method, "-")
	cmd.Stdin = bytes.NewReader(p)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return nil, &RPCError{Code: "cli_error", Message: msg}
		}
		return nil, err
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		out = []byte("{}")
	}
	if !json.Valid(out) {
		return nil, fmt.Errorf("cmux rpc %s: non-JSON output %q", method, truncate(string(out), 200))
	}
	return out, nil
}

func isRPCError(err error) bool {
	var r *RPCError
	return errors.As(err, &r)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
