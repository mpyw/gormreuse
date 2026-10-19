package cfg

import (
	"testing"
)

func TestCanReach(t *testing.T) {
	t.Parallel()
	// entry branches to two exclusive blocks that merge at the return.
	fn := buildFunc(t, "package p\nfunc f(c bool) int {\n\tvar x int\n\tif c {\n\t\tx = 1\n\t} else {\n\t\tx = 2\n\t}\n\treturn x\n}", "f")
	a := NewAnalyzer()

	entry := fn.Blocks[0]
	last := fn.Blocks[len(fn.Blocks)-1]

	if !a.CanReach(entry, entry) {
		t.Error("a block must reach itself")
	}
	if !a.CanReach(entry, last) {
		t.Error("entry should reach the final block")
	}
	if a.CanReach(last, entry) {
		t.Error("the final block should not reach the entry (no back-edge)")
	}
	if a.CanReach(nil, entry) || a.CanReach(entry, nil) {
		t.Error("nil blocks are never reachable")
	}

	// The two exclusive branches must not reach each other.
	if len(entry.Succs) == 2 {
		thenB, elseB := entry.Succs[0], entry.Succs[1]
		if a.CanReach(thenB, elseB) || a.CanReach(elseB, thenB) {
			t.Error("exclusive if/else branches must not reach each other")
		}
	}
}
