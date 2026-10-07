package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/savvychez/ray/internal/cmux"
)

// fakeCmuxSocket serves a minimal cmux v2 socket with nWorkspaces
// workspaces, enforcing cmux's per-connection polling limiter (burst 9,
// one token per 100ms).
func fakeCmuxSocket(t *testing.T, nWorkspaces int, withSystemTree bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmux.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	wsID := func(i int) string { return fmt.Sprintf("WS-%02d", i) }
	surface := func(i int) map[string]any {
		return map[string]any{"id": fmt.Sprintf("SF-%02d", i), "type": "terminal", "title": "zsh", "focused": true}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var mu sync.Mutex
				tokens, last := 9, time.Now()
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadBytes('\n')
					if err != nil {
						return
					}
					var req struct {
						ID     int            `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					json.Unmarshal(line, &req)
					resp := map[string]any{"id": req.ID, "ok": true}
					mu.Lock()
					if n := int(time.Since(last) / (100 * time.Millisecond)); n > 0 {
						tokens, last = min(9, tokens+n), time.Now()
					}
					limited := tokens == 0
					if !limited {
						tokens--
					}
					mu.Unlock()
					switch {
					case limited:
						resp = map[string]any{"id": req.ID, "ok": false, "error": map[string]any{
							"code": "rate_limited", "message": "Polling rate limited for this connection",
							"data": map[string]any{"retry_after_ms": 100},
						}}
					case req.Method == "system.ping":
						resp["result"] = map[string]any{}
					case req.Method == "system.tree" && withSystemTree:
						var wss []any
						for i := 0; i < nWorkspaces; i++ {
							wss = append(wss, map[string]any{"id": wsID(i), "index": i, "title": fmt.Sprint("ws ", i), "selected": i == 0,
								"panes": []any{map[string]any{"focused": true, "surfaces": []any{surface(i)}}}})
						}
						resp["result"] = map[string]any{"windows": []any{map[string]any{"key": false, "workspaces": wss}}}
					case req.Method == "workspace.group.list":
						resp["result"] = map[string]any{"groups": []any{map[string]any{
							"id": "G1", "name": "defects", "is_collapsed": true,
							"anchor_workspace_id": wsID(1), "member_workspace_ids": []any{wsID(1), wsID(2)},
						}}}
					case req.Method == "workspace.list":
						var wss []any
						for i := 0; i < nWorkspaces; i++ {
							wss = append(wss, map[string]any{"id": wsID(i), "index": i, "title": fmt.Sprint("ws ", i), "selected": i == 0})
						}
						resp["result"] = map[string]any{"workspaces": wss}
					case req.Method == "surface.list":
						var i int
						fmt.Sscanf(fmt.Sprint(req.Params["workspace_id"]), "WS-%d", &i)
						resp["result"] = map[string]any{"surfaces": []any{surface(i)}}
					default:
						resp = map[string]any{"id": req.ID, "ok": false, "error": map[string]any{"code": "method_not_found", "message": "Unknown method"}}
					}
					b, _ := json.Marshal(resp)
					c.Write(append(b, '\n'))
				}
			}()
		}
	}()
	return path
}

func TestCmuxTreeUnderRateLimit(t *testing.T) {
	for _, withSystemTree := range []bool{true, false} {
		t.Run(fmt.Sprintf("systemTree=%v", withSystemTree), func(t *testing.T) {
			path := fakeCmuxSocket(t, 21, withSystemTree)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := cmux.Dial(ctx, "socket", path, "")
			if err != nil {
				t.Fatal(err)
			}
			b := &CmuxBackend{C: c}
			// Poll repeatedly, like a session does.
			for round := 0; round < 3; round++ {
				tree, err := b.Tree(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(tree) != 21 {
					t.Fatalf("got %d workspaces", len(tree))
				}
				for _, ws := range tree {
					if ws.Error != "" || len(ws.Surfaces) != 1 {
						t.Fatalf("round %d: workspace %s: surfaces=%v error=%q", round, ws.Title, ws.Surfaces, ws.Error)
					}
				}
				if g := tree[1].Group; g == nil || g.Name != "defects" || !g.Anchor || !g.Collapsed {
					t.Errorf("anchor group = %+v", g)
				}
				if g := tree[2].Group; g == nil || g.ID != "G1" || g.Anchor {
					t.Errorf("member group = %+v", g)
				}
				if tree[0].Group != nil || tree[3].Group != nil {
					t.Errorf("ungrouped workspaces got a group")
				}
				if !tree[0].Selected {
					t.Errorf("first workspace should be frontmost when no window is key")
				}
			}
		})
	}
}
