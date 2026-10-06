package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// closingServer answers each request, then hangs up after `perConn`
// requests on a connection, like cmux dropping an idle client.
func closingServer(t *testing.T, perConn int) (path string, handled *atomic.Int32) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	handled = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				rd := bufio.NewReader(c)
				for i := 0; i < perConn; i++ {
					line, err := rd.ReadBytes('\n')
					if err != nil {
						return
					}
					var req struct {
						ID int `json:"id"`
					}
					json.Unmarshal(line, &req)
					handled.Add(1)
					b, _ := json.Marshal(map[string]any{"id": req.ID, "ok": true, "result": map[string]any{}})
					c.Write(append(b, '\n'))
				}
			}()
		}
	}()
	return path, handled
}

func TestSocketSurvivesServerHangup(t *testing.T) {
	path, _ := closingServer(t, 1)
	s := &SocketCaller{Path: path}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.Call(ctx, "surface.read_text", map[string]any{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond) // let the server's close land
	}
}

func TestSocketRedialsAfterIdle(t *testing.T) {
	old := maxIdle
	maxIdle = 50 * time.Millisecond
	defer func() { maxIdle = old }()
	path, handled := closingServer(t, 1)
	s := &SocketCaller{Path: path}
	defer s.Close()
	ctx := context.Background()
	// Non-polling calls aren't retried after a failed read, so these only
	// pass because the idle connection is replaced before use.
	for i := 0; i < 3; i++ {
		if _, err := s.Call(ctx, "surface.send_text", map[string]any{"text": "x"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := handled.Load(); n != 3 {
		t.Fatalf("server handled %d sends, want exactly 3 (no duplicates)", n)
	}
}
