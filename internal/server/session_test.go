package server

import (
	"fmt"
	"reflect"
	"testing"
)

func apply(prev []string, drop, keep int, add []string) []string {
	return append(append([]string{}, prev[drop:drop+keep]...), add...)
}

func TestDiffLines(t *testing.T) {
	seq := func(from, to int) []string {
		var s []string
		for i := from; i < to; i++ {
			s = append(s, fmt.Sprint("line ", i))
		}
		return s
	}
	tests := []struct {
		name       string
		prev, next []string
		maxSent    int // most lines we're willing to resend
	}{
		{"first", nil, seq(0, 10), 10},
		{"append", seq(0, 10), seq(0, 12), 2},
		{"scroll window", seq(0, 100), seq(5, 105), 5},
		{"redraw last line", append(seq(0, 10), "$ ec"), append(seq(0, 10), "$ echo"), 1},
		{"clear", seq(0, 50), []string{"$"}, 1},
		{"same", seq(0, 5), seq(0, 5), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drop, keep := diffLines(tt.prev, tt.next)
			sent := tt.next[keep:]
			if len(sent) > tt.maxSent {
				t.Errorf("sent %d lines, want <= %d (drop=%d keep=%d)", len(sent), tt.maxSent, drop, keep)
			}
			prev := tt.prev
			if prev == nil {
				prev = []string{}
			}
			if got := apply(prev, drop, keep, sent); !reflect.DeepEqual(got, tt.next) && !(len(got) == 0 && len(tt.next) == 0) {
				t.Errorf("apply = %q, want %q", got, tt.next)
			}
		})
	}
}

func TestBase32Code(t *testing.T) {
	p := &Pairing{}
	a, b := p.NewCode(0), p.NewCode(0)
	if a == b || len(a) != 26 {
		t.Fatalf("codes %q %q", a, b)
	}
}
