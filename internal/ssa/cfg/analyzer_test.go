package cfg

import (
	"testing"

	"golang.org/x/tools/go/ssa"
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

// TestCanReachOutsideTheCache covers blocks that the reachability cache does
// not index: blocks of two functions, and blocks built by hand with no
// function, which are searched instead.
func TestCanReachOutsideTheCache(t *testing.T) {
	t.Parallel()
	f := buildFunc(t, "package p\nfunc f(n int) int { for n > 0 { n-- }; return n }", "f")
	g := buildFunc(t, "package p\nfunc g(n int) int { for n > 0 { n-- }; return n }", "g")
	a := NewAnalyzer()
	if a.CanReach(f.Blocks[0], g.Blocks[1]) {
		t.Error("a block of f must not reach a block of g")
	}

	src, dst := &ssa.BasicBlock{}, &ssa.BasicBlock{}
	mid := &ssa.BasicBlock{Succs: []*ssa.BasicBlock{dst}}
	src.Succs = []*ssa.BasicBlock{mid, src}
	if !a.CanReach(src, dst) {
		t.Error("a hand-built block should reach its successor's successor")
	}
	if a.CanReach(dst, src) {
		t.Error("a hand-built block with no successors should reach nothing")
	}
}

// TestAnalyzerCaches guards #299: the loops and the Repeats of a function are
// found once, and the reachability of a block is kept.
func TestAnalyzerCaches(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, "package p\nfunc f(n int) int {\n\ts := 0\n\tg := func() { s++ }\n\tfor i := 0; i < n; i++ {\n\t\tg()\n\t}\n\treturn s\n}", "f")
	a := NewAnalyzer()
	if loops := a.DetectLoops(fn); a.DetectLoops(fn) != loops {
		t.Error("DetectLoops should return the cached LoopInfo")
	}
	if a.Repeats(fn) != a.RepeatsOf(fn) {
		t.Error("RepeatsOf a top-level function should be its own Repeats")
	}
	closure := fn.AnonFuncs[0]
	if a.RepeatsOf(closure) != a.Repeats(fn).Closure(closure) {
		t.Error("RepeatsOf a closure should be the cached Closure of its parent")
	}
	if a.RepeatsOf(closure) == a.Repeats(closure) {
		t.Error("a closure analysed on its own should not share the Repeats that asks its parent")
	}
	// The closure is called in the loop, so its use repeats after the entry.
	// The second question reads the memo.
	for range 2 {
		if !a.RepeatsOf(closure).RepeatsAfter(closure.Blocks[0], fn.Blocks[0]) {
			t.Error("a closure called in a loop should repeat after the entry")
		}
	}
	last := fn.Blocks[len(fn.Blocks)-1]
	for range 2 {
		if !a.CanReach(fn.Blocks[0], last) || a.CanReach(last, fn.Blocks[0]) {
			t.Error("the entry should reach the return, and not the reverse")
		}
	}
}
