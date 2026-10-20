package cfg

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestRepeatsAfter(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, "package p\nfunc f(n int) int { s := 0; for i := 0; i < n; i++ { s += i }; return s }", "f")
	other := buildFunc(t, "package p\nfunc g(n int) int { for n > 0 { n-- }; return n }", "g")
	a := NewAnalyzer()
	info := a.DetectLoops(fn)
	r := a.Repeats(fn)

	entry := fn.Blocks[0]
	body := repeatTestLoopBlock(t, fn, info)

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
		// A def in a function that does not declare fn is outside fn's loops.
		{"use in the loop, def in another function", body, other.Blocks[1], true},
		{"use outside the loop, def in another function", entry, other.Blocks[1], false},
	}
	for _, tc := range tests {
		if got := r.RepeatsAfter(tc.use, tc.def); got != tc.want {
			t.Errorf("%s: RepeatsAfter = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRepeatsAfterClosure covers a use in a closure whose def is in the
// function that declares it (#249), and a def in a closure (an IIFE) that
// the use's function runs.
func TestRepeatsAfterClosure(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, `package p
func f(n int) int {
	s := 0
	twice := func() { s++ }
	twice()
	twice()
	inLoop := func() { s-- }
	once := func() { s += 2 }
	once()
	exclusive := func() { s += 3 }
	if n > 1 {
		exclusive()
	} else {
		exclusive()
	}
	stored := func() { s += 4 }
	var held func()
	for i := 0; i < n; i++ {
		inLoop()
		held = stored
		func() { s += 5 }()
	}
	held()
	nested := func() { func() { s += 6 }() }
	nested()
	nested()
	twiceStored := func() { s += 7 }
	var h func()
	if n > 0 {
		h = twiceStored
	} else {
		h = twiceStored
	}
	func() { h() }()
	return s
}`, "f")
	a := NewAnalyzer()
	info := a.DetectLoops(fn)
	r := a.Repeats(fn)
	entry := fn.Blocks[0]
	body := repeatTestLoopBlock(t, fn, info)

	closure := func(i int) (*Repeats, *ssa.BasicBlock) {
		c := fn.AnonFuncs[i]
		return r.Closure(c), c.Blocks[0]
	}
	tests := []struct {
		name    string
		closure int
		want    bool
	}{
		{"called twice", 0, true},
		{"called in a loop", 1, true},
		{"called once", 2, false},
		{"called on each of two exclusive branches", 3, false},
		{"stored in a loop to a variable called after it", 4, false},
		{"IIFE in a loop", 5, true},
		// Two stores to one captured variable, read by one call.
		{"stored twice, called once", 7, false},
	}
	for _, tc := range tests {
		c, use := closure(tc.closure)
		if got := c.RepeatsAfter(use, entry); got != tc.want {
			t.Errorf("%s, def before it: RepeatsAfter = %v, want %v", tc.name, got, tc.want)
		}
		// A def in the closure itself is defined on each run.
		if c.RepeatsAfter(use, use) {
			t.Errorf("%s, def in the closure: RepeatsAfter = true, want false", tc.name)
		}
	}

	// The IIFE in the loop runs once per iteration: a def in the loop body
	// runs as often.
	if c, use := closure(5); c.RepeatsAfter(use, body) {
		t.Error("IIFE in a loop, def in the loop body: RepeatsAfter = true, want false")
	}

	// A closure in a closure called twice runs twice.
	nested := fn.AnonFuncs[6]
	nr := r.Closure(nested)
	inner := nested.AnonFuncs[0]
	if !nr.Closure(inner).RepeatsAfter(inner.Blocks[0], entry) {
		t.Error("IIFE in a closure called twice: RepeatsAfter = false, want true")
	}

	// A def in the IIFE of the loop body is taken where the IIFE runs.
	iife := fn.AnonFuncs[5].Blocks[0]
	if r.RepeatsAfter(body, iife) {
		t.Error("use in the loop body, def in its IIFE: RepeatsAfter = true, want false")
	}
	// A def in a closure that runs at two sites is taken where it is declared.
	if !r.RepeatsAfter(body, fn.AnonFuncs[0].Blocks[0]) {
		t.Error("use in the loop body, def in a closure declared before it: RepeatsAfter = false, want true")
	}
}

func TestRunsAgain(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, "package p\nfunc f(n int) int { s := 0; for i := 0; i < n; i++ { s += i }; return s }", "f")
	a := NewAnalyzer()
	info := a.DetectLoops(fn)
	r := a.Repeats(fn)
	if r.RunsAgain(fn.Blocks[0]) {
		t.Error("entry: RunsAgain = true, want false")
	}
	if !r.RunsAgain(repeatTestLoopBlock(t, fn, info)) {
		t.Error("loop body: RunsAgain = false, want true")
	}
}

// repeatTestLoopBlock returns the first block of fn in a loop.
func repeatTestLoopBlock(t *testing.T, fn *ssa.Function, info *LoopInfo) *ssa.BasicBlock {
	t.Helper()
	for _, b := range fn.Blocks {
		if info.IsInLoop(b) {
			return b
		}
	}
	t.Fatal("expected a block in the loop")
	return nil
}
