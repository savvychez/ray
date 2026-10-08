package bob

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Msg is a bob message in the shape the phone renders.
type Msg struct {
	Seq   int64   `json:"seq"`
	Role  string  `json:"role"` // user, assistant, tool, info, error, system
	Text  string  `json:"text,omitempty"`
	Calls []Call  `json:"calls,omitempty"`
	Res   *Result `json:"result,omitempty"`
	Time  int64   `json:"time"`
	Cost  float64 `json:"cost,omitempty"`
	Stop  string  `json:"stop,omitempty"` // e.g. user_cancelled
}

// Call is a tool call an assistant message makes.
type Call struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Result is a tool's outcome, matched to its Call by ID.
type Result struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name,omitempty"`
	Display string `json:"display,omitempty"` // bob's label, e.g. "Execute Command"
	Error   bool   `json:"error,omitempty"`
	Output  string `json:"output,omitempty"`
}

// Limits on what is sent to the phone: bob's tool outputs and file
// contents can be huge.
const (
	maxText   = 20000
	maxOutput = 4000
	maxArg    = 1500
)

// raw mirrors the JSON in messages.data.
type raw struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"toolCalls"`
	ToolUsage *struct {
		Signature struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			IsError bool   `json:"isError"`
		} `json:"signature"`
		Labels struct {
			DisplayName string `json:"displayName"`
		} `json:"labels"`
	} `json:"toolUsage"`
	Meta struct {
		Timestamp  int64  `json:"timestamp"`
		StopReason string `json:"stopReason"`
		Spend      struct {
			Cost float64 `json:"cost"`
		} `json:"spend"`
	} `json:"_meta"`
}

// Convert turns stored rows into chat messages. Rows it can't parse are
// kept as plain text so nothing silently disappears.
func Convert(rows []Row) []Msg {
	out := make([]Msg, 0, len(rows))
	for _, r := range rows {
		if r.Role == "system" || r.Role == "developer" {
			continue // bob's system prompt: long, and not part of the conversation
		}
		m := Msg{Seq: r.Seq, Role: r.Role, Time: r.CreatedAt}
		var d raw
		if err := json.Unmarshal([]byte(r.Data), &d); err != nil {
			m.Text = clip(r.Data, maxText)
			out = append(out, m)
			continue
		}
		if d.Meta.Timestamp > 0 {
			m.Time = d.Meta.Timestamp
		}
		m.Cost = d.Meta.Spend.Cost
		m.Stop = d.Meta.StopReason
		text := contentText(d.Content)
		switch r.Role {
		case "tool":
			res := &Result{Output: clip(text, maxOutput)}
			if u := d.ToolUsage; u != nil {
				res.CallID = u.Signature.ID
				res.Name = u.Signature.Name
				res.Error = u.Signature.IsError
				res.Display = u.Labels.DisplayName
			}
			m.Res = res
		default:
			m.Text = clip(strings.TrimSpace(text), maxText)
			for _, c := range d.ToolCalls {
				m.Calls = append(m.Calls, Call{ID: c.ID, Name: c.Name, Args: trimJSON(c.Arguments)})
			}
		}
		// Skip empty assistant messages that carry nothing to show.
		if m.Role == "assistant" && m.Text == "" && len(m.Calls) == 0 && m.Stop == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// contentText flattens a message's content: a string, or a list of parts
// ({type:"text", text}, images, …).
func contentText(c json.RawMessage) string {
	if len(c) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var parts []map[string]any
	if json.Unmarshal(c, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			switch p["type"] {
			case "text":
				if t, ok := p["text"].(string); ok {
					if b.Len() > 0 {
						b.WriteString("\n")
					}
					b.WriteString(t)
				}
			case "image", "image_url":
				b.WriteString("[image]")
			}
		}
		return b.String()
	}
	return string(c)
}

// trimJSON shortens long string values inside tool arguments (file
// contents, diffs) so a single call can't flood the phone.
func trimJSON(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(r, &v) != nil {
		return json.RawMessage(mustJSON(clip(string(r), maxArg)))
	}
	b, err := json.Marshal(trimValue(v))
	if err != nil {
		return nil
	}
	return b
}

func trimValue(v any) any {
	switch x := v.(type) {
	case string:
		return clip(x, maxArg)
	case []any:
		for i := range x {
			x[i] = trimValue(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = trimValue(x[k])
		}
	}
	return v
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// clip truncates s to about n bytes on a rune boundary, noting the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (" + itoa(len(s)-cut) + " more bytes)"
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// State summarizes what the session is doing for the status pill.
type State string

const (
	StateWorking  State = "working"  // bob is thinking or running a tool
	StateApproval State = "approval" // waiting on a permission prompt
	StateIdle     State = "idle"     // bob finished its turn; your move
	StateStopped  State = "stopped"  // no bob process holds the session
)

// StateOf derives the session state from the newest messages and any
// pending approvals.
func StateOf(t *Task, last []Msg, approvals []Approval) State {
	if len(approvals) > 0 {
		return StateApproval
	}
	if t != nil && !t.Live {
		return StateStopped
	}
	if len(last) == 0 {
		return StateIdle
	}
	m := last[len(last)-1]
	switch {
	case m.Role == "assistant" && len(m.Calls) == 0:
		return StateIdle // answered without calling tools: your turn
	case m.Role == "assistant" && m.Stop != "":
		return StateIdle // cancelled mid-call
	case m.Role == "error":
		return StateIdle
	default:
		return StateWorking // a user message or tool result bob is processing, or a running tool
	}
}

// TrimPayload shortens an approval payload for the phone.
func TrimPayload(p json.RawMessage) json.RawMessage {
	return trimJSON(p)
}
