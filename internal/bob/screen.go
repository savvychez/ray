package bob

import "strings"

// Prompt is bob's permission menu as drawn in its terminal. bob 2.0.5
// doesn't record these in task_pending_approvals, so the screen is the
// only reliable sign that it is waiting:
//
//	──────────────────────
//	Execute Command
//	──────────────────────
//	Command:      date
//	Approve commands:
//	┌──────┐
//	│ date │
//	└──────┘
//	→ Approve Once
//	  Always Allow Command for task
//	  Reject
//	↑↓ (1/3)
//	Press Enter to confirm
type Prompt struct {
	Title   string   `json:"title,omitempty"`
	Detail  []string `json:"detail,omitempty"`
	Options []string `json:"options"`
	Cursor  int      `json:"cursor"` // index of the highlighted option
}

// Key identifies a prompt, to tell a new prompt from the same one redrawn.
func (p *Prompt) Key() string {
	if p == nil {
		return ""
	}
	return "screen:" + p.Title + "|" + strings.Join(p.Detail, "|")
}

// Index finds the option for a choice: once, always or reject.
func (p *Prompt) Index(choice string) int {
	for i, o := range p.Options {
		l := strings.ToLower(o)
		switch {
		case choice == "once" && strings.HasPrefix(l, "approve once"),
			choice == "reject" && strings.HasPrefix(l, "reject"),
			choice == "always" && (strings.HasPrefix(l, "always") || strings.HasPrefix(l, "approve") && !strings.HasPrefix(l, "approve once")):
			return i
		}
	}
	return -1
}

// promptTail is how close to the bottom of the screen the menu must be:
// further up, it is history that has scrolled by.
const promptTail = 12

// ParseScreen finds bob's permission menu at the bottom of a terminal
// screen, or returns nil.
func ParseScreen(text string) *Prompt {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	first := -1
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-promptTail-6; i-- {
		if _, opt := menuLine(lines[i]); strings.HasPrefix(strings.ToLower(opt), "approve once") {
			first = i
			break
		}
	}
	if first < 0 || nonEmptyAfter(lines, first) > promptTail {
		return nil
	}
	p := &Prompt{}
	for i := first; i < len(lines); i++ {
		sel, opt := menuLine(lines[i])
		if opt == "" || strings.HasPrefix(opt, "↑↓") {
			break
		}
		if sel {
			p.Cursor = len(p.Options)
		}
		p.Options = append(p.Options, opt)
	}
	if p.Index("reject") < 0 {
		return nil
	}
	// Above the options: detail lines up to a rule, then the title just
	// above that rule.
	i := first - 1
	for ; i >= 0 && first-i < 30 && !isRule(lines[i]); i-- {
		if d := strings.Trim(strings.TrimSpace(lines[i]), "│┃|┌┐└┘─━ "); d != "" {
			p.Detail = append([]string{d}, p.Detail...)
		}
	}
	for i--; i >= 0 && first-i < 34 && !isRule(lines[i]); i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			p.Title = t
			break
		}
	}
	return p
}

// menuLine splits a menu line into its selection marker and text.
func menuLine(l string) (selected bool, text string) {
	t := strings.TrimSpace(l)
	for _, m := range []string{"→", "❯", "›", ">", "▸", "●"} {
		if strings.HasPrefix(t, m) {
			return true, strings.TrimSpace(strings.TrimPrefix(t, m))
		}
	}
	return false, t
}

func isRule(l string) bool {
	t := strings.TrimSpace(l)
	if len([]rune(t)) < 8 {
		return false
	}
	for _, r := range t {
		if r != '─' && r != '━' && r != '-' && r != '═' {
			return false
		}
	}
	return true
}

func nonEmptyAfter(lines []string, i int) int {
	n := 0
	for _, l := range lines[i+1:] {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}
