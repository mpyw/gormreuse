package cfg

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestRepeatsAfter(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, "package p\nfunc f(n int) int { s := 0; for i := 0; i < n; i++ { s += i }; return s }", "f")
	other := buildFunc(t, "package p\nfunc g(n int) int { for n > 0 { n-- }; return n }", "g")
	info := NewAnalyzer().DetectLoops(fn)
	r := NewRepeats(info)

	entry := fn.Blocks[0]
	body := entry
	for _, b := range fn.Blocks {
		if info.IsInLoop(b) {
			body = b
			break
		}
	}
	if body == entry {
		t.Fatal("expected a block in the loop")
	}

	tests := []struct {
		name     string
		use, def *ssa.BasicBlock
		want     bool
	}{
		{"use in the loop, def before it", body, entry, true},
		{"use in the loop, no def", body, nil, true},
		{"use and def in the loop", body, body, false},
		{"use outside the loop", entry, entry, false},
		{"use outside the loop, no def", entry, nil, false},
		// Only the use's function's loops are known: a def in another
		// function, even in a loop there, is outside them.
		{"use in the loop, def in another function", body, other.Blocks[1], true},
	}
	for _, tc := range tests {
		if got := r.RepeatsAfter(tc.use, tc.def); got != tc.want {
			t.Errorf("%s: RepeatsAfter = %v, want %v", tc.name, got, tc.want)
		}
	}
}
