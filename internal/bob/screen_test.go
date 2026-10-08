package bob

import (
	"strings"
	"testing"
)

const approvalScreen = `> try again

 ────────────────────────────────────────────
 Execute Command
 ────────────────────────────────────────────
 Command:          date
 Approve commands:
 ┌──────┐
 │ date │
 └──────┘
 → Approve Once
   Always Allow Command for task
   Reject

   ↑↓ (1/3)

 Wait for input after execution: No (Tab to toggle)

 Press Enter to confirm
 ────────────────────────────────────────────

`

func TestParseScreen(t *testing.T) {
	p := ParseScreen(approvalScreen)
	if p == nil {
		t.Fatal("no prompt found")
	}
	if p.Title != "Execute Command" {
		t.Errorf("title = %q", p.Title)
	}
	if got := strings.Join(p.Detail, "|"); got != "Command:          date|Approve commands:|date" {
		t.Errorf("detail = %q", got)
	}
	if got := strings.Join(p.Options, "|"); got != "Approve Once|Always Allow Command for task|Reject" || p.Cursor != 0 {
		t.Errorf("options = %q cursor %d", got, p.Cursor)
	}
	if p.Index("once") != 0 || p.Index("always") != 1 || p.Index("reject") != 2 {
		t.Errorf("index: %d %d %d", p.Index("once"), p.Index("always"), p.Index("reject"))
	}

	moved := strings.Replace(strings.Replace(approvalScreen, "→ Approve Once", "  Approve Once", 1), "   Reject", " → Reject", 1)
	if p := ParseScreen(moved); p == nil || p.Cursor != 2 {
		t.Errorf("cursor after moving: %+v", p)
	}

	// The same menu scrolled up by later output is history, not a prompt.
	if p := ParseScreen(approvalScreen + strings.Repeat("output line\n", 20)); p != nil {
		t.Errorf("stale menu matched: %+v", p)
	}
	if p := ParseScreen("$ ls\nApprove Once is a nice phrase\n$ "); p != nil {
		t.Errorf("menu without Reject matched: %+v", p)
	}
}
