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
	Code    string `json:"code"`
	Message string `json:"message"`
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

	mu   sync.Mutex
	conn net.Conn
	rd   *bufio.Reader
	next int
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
	res, err := s.callLocked(ctx, method, params)
	if err != nil && !isRPCError(err) && s.conn != nil {
		// Drop the connection; the next call redials.
		s.conn.Close()
		s.conn = nil
	}
	return res, err
}

func (s *SocketCaller) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
		return nil, err
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
