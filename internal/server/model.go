package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/savvychez/ray/internal/cmux"
)

// Workspace is a cmux workspace (a sidebar entry) and its terminals.
type Workspace struct {
	ID       string    `json:"id"`
	Index    int       `json:"index"`
	Title    string    `json:"title"`
	Selected bool      `json:"selected"`
	Surfaces []Surface `json:"surfaces"`
	// Error is set when cmux couldn't list this workspace's surfaces.
	Error string `json:"error,omitempty"`
	// Group is the cmux sidebar group (folder) this workspace belongs to.
	Group *GroupRef `json:"group,omitempty"`
}

// GroupRef places a workspace in a cmux workspace group. A group's header
// row is itself a workspace (the anchor), which can have its own terminals.
type GroupRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Collapsed bool   `json:"collapsed,omitempty"`
	Anchor    bool   `json:"anchor,omitempty"` // this workspace is the header
}

// Surface is a tab/panel inside a workspace.
type Surface struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Title   string `json:"title"`
	Type    string `json:"type,omitempty"`
	Focused bool   `json:"focused"`
	TTY     string `json:"tty,omitempty"`
	Bob     bool   `json:"bob,omitempty"` // running IBM Bob Shell; shown as a chat
}

// Backend is what a session needs from cmux. It is an interface so the
// server can run against a fake for demos and tests.
type Backend interface {
	Tree(ctx context.Context) ([]Workspace, error)
	ReadText(ctx context.Context, workspaceID, surfaceID string, lines int) (string, error)
	SendText(ctx context.Context, workspaceID, surfaceID, text string) error
	SendKey(ctx context.Context, workspaceID, surfaceID, key string) error
	NewWorkspace(ctx context.Context) (string, error)
	Focus(ctx context.Context, workspaceID, surfaceID string) error
	Name() string
}

// CmuxBackend implements Backend on top of the cmux v2 API.
type CmuxBackend struct {
	C cmux.Caller

	noSystemTree bool // set once cmux says it lacks system.tree
}

func (b *CmuxBackend) Name() string { return b.C.Name() }

func (b *CmuxBackend) call(ctx context.Context, method string, params map[string]any, out any) error {
	raw, err := b.C.Call(ctx, method, params)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Tree returns every workspace in every window with its surfaces. It uses
// one system.tree call: cmux rate-limits polling per connection (a burst of
// 9 calls), so a surface.list per workspace starves on bigger setups.
func (b *CmuxBackend) Tree(ctx context.Context) ([]Workspace, error) {
	if !b.noSystemTree {
		ws, err := b.systemTree(ctx)
		if err == nil {
			return ws, nil
		}
		var rerr *cmux.RPCError
		if !errors.As(err, &rerr) || (rerr.Code != "method_not_found" && rerr.Code != "invalid_params") {
			return nil, err
		}
		b.noSystemTree = true // older cmux; fall back for good
	}
	return b.listTree(ctx)
}

func (b *CmuxBackend) systemTree(ctx context.Context) ([]Workspace, error) {
	var t struct {
		Windows []struct {
			ID         string `json:"id"`
			Key        bool   `json:"key"`
			Workspaces []struct {
				ID       string `json:"id"`
				Index    int    `json:"index"`
				Title    string `json:"title"`
				Selected bool   `json:"selected"`
				Panes    []struct {
					Surfaces []Surface `json:"surfaces"`
				} `json:"panes"`
			} `json:"workspaces"`
		} `json:"windows"`
	}
	if err := b.call(ctx, "system.tree", map[string]any{"all_windows": true}, &t); err != nil {
		return nil, err
	}
	// "Frontmost" is the selected workspace of the key window, or of the
	// first window when cmux isn't the active app (no window is key then).
	front := 0
	for i, win := range t.Windows {
		if win.Key {
			front = i
			break
		}
	}
	out := []Workspace{}
	for wi, win := range t.Windows {
		for _, w := range win.Workspaces {
			ws := Workspace{ID: w.ID, Index: w.Index, Title: w.Title, Selected: w.Selected && wi == front}
			seen := map[string]bool{}
			for _, p := range w.Panes {
				for _, s := range p.Surfaces {
					if seen[s.ID] {
						continue
					}
					seen[s.ID] = true
					s.Index = len(ws.Surfaces)
					ws.Surfaces = append(ws.Surfaces, s)
				}
			}
			out = append(out, ws)
		}
	}
	var windowIDs []string
	for _, win := range t.Windows {
		windowIDs = append(windowIDs, win.ID)
	}
	b.applyGroups(ctx, out, windowIDs)
	return out, nil
}

// applyGroups tags workspaces with their sidebar group. Groups are per
// window. Failures (e.g. a cmux without groups) just leave them ungrouped.
func (b *CmuxBackend) applyGroups(ctx context.Context, wss []Workspace, windowIDs []string) {
	byID := map[string]*Workspace{}
	for i := range wss {
		byID[wss[i].ID] = &wss[i]
	}
	if len(windowIDs) == 0 {
		windowIDs = []string{""}
	}
	for _, win := range windowIDs {
		params := map[string]any{}
		if win != "" {
			params["window_id"] = win
		}
		var gl struct {
			Groups []struct {
				ID        string   `json:"id"`
				Name      string   `json:"name"`
				Collapsed bool     `json:"is_collapsed"`
				Anchor    string   `json:"anchor_workspace_id"`
				Members   []string `json:"member_workspace_ids"`
			} `json:"groups"`
		}
		if err := b.call(ctx, "workspace.group.list", params, &gl); err != nil {
			continue
		}
		for _, g := range gl.Groups {
			members := append([]string{}, g.Members...)
			if g.Anchor != "" {
				members = append(members, g.Anchor)
			}
			for _, id := range members {
				if ws := byID[id]; ws != nil {
					ws.Group = &GroupRef{ID: g.ID, Name: g.Name, Collapsed: g.Collapsed, Anchor: id == g.Anchor}
				}
			}
		}
	}
}

// listTree is the pre-system.tree fallback: workspace.list plus one
// surface.list per workspace.
func (b *CmuxBackend) listTree(ctx context.Context) ([]Workspace, error) {
	var wl struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := b.call(ctx, "workspace.list", map[string]any{}, &wl); err != nil {
		return nil, err
	}
	for i := range wl.Workspaces {
		ws := &wl.Workspaces[i]
		var sl struct {
			Surfaces []Surface `json:"surfaces"`
		}
		if err := b.call(ctx, "surface.list", map[string]any{"workspace_id": ws.ID}, &sl); err != nil {
			// Not fatal: the phone can still open the workspace and let
			// cmux pick its focused terminal, and shows this reason.
			ws.Error = err.Error()
			continue
		}
		ws.Surfaces = sl.Surfaces
	}
	b.applyGroups(ctx, wl.Workspaces, nil)
	return wl.Workspaces, nil
}

func target(workspaceID, surfaceID string) map[string]any {
	p := map[string]any{}
	if workspaceID != "" {
		p["workspace_id"] = workspaceID
	}
	if surfaceID != "" {
		p["surface_id"] = surfaceID
	}
	return p
}

func (b *CmuxBackend) ReadText(ctx context.Context, workspaceID, surfaceID string, lines int) (string, error) {
	p := target(workspaceID, surfaceID)
	if lines > 0 {
		p["lines"] = lines
	}
	var r struct {
		Text string `json:"text"`
	}
	err := b.call(ctx, "surface.read_text", p, &r)
	return r.Text, err
}

func (b *CmuxBackend) SendText(ctx context.Context, workspaceID, surfaceID, text string) error {
	p := target(workspaceID, surfaceID)
	p["text"] = text
	return b.call(ctx, "surface.send_text", p, nil)
}

func (b *CmuxBackend) SendKey(ctx context.Context, workspaceID, surfaceID, key string) error {
	p := target(workspaceID, surfaceID)
	p["key"] = key
	return b.call(ctx, "surface.send_key", p, nil)
}

func (b *CmuxBackend) NewWorkspace(ctx context.Context) (string, error) {
	var r struct {
		WorkspaceID string `json:"workspace_id"`
		ID          string `json:"id"`
	}
	if err := b.call(ctx, "workspace.create", map[string]any{}, &r); err != nil {
		return "", err
	}
	if r.WorkspaceID != "" {
		return r.WorkspaceID, nil
	}
	return r.ID, nil
}

func (b *CmuxBackend) Focus(ctx context.Context, workspaceID, surfaceID string) error {
	if workspaceID != "" {
		if err := b.call(ctx, "workspace.select", map[string]any{"workspace_id": workspaceID}, nil); err != nil {
			return err
		}
	}
	if surfaceID != "" {
		return b.call(ctx, "surface.focus", map[string]any{"surface_id": surfaceID}, nil)
	}
	return nil
}
