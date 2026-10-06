package server

import (
	"context"
	"encoding/json"
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
}

// Surface is a tab/panel inside a workspace.
type Surface struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Title   string `json:"title"`
	Type    string `json:"type,omitempty"`
	Focused bool   `json:"focused"`
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

func (b *CmuxBackend) Tree(ctx context.Context) ([]Workspace, error) {
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
