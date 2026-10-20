package cfg

import (
	"strings"
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

// repeatClosureSitesSource marks each closure under test with a comment on
// the line of its literal.
const repeatClosureSitesSource = `package p

func f(n, k int, c bool) int {
	s := 0
	update := func() { s++ } // calledInLoopOfClosure
	run := func() {
		for i := 0; i < n; i++ {
			update()
		}
	}
	run()
	once := func() { s-- } // calledOnceByClosure
	callOnce := func() { once() }
	callOnce()
	twice := func() { s += 2 } // calledTwiceInIIFE
	func() { twice(); twice() }()
	var walk func(k int)
	walk = func(k int) { // recursive
		s += k
		if k > 0 {
			walk(k - 1)
		}
	}
	walk(n)
	var even, odd func(k int)
	even = func(k int) { // mutual
		s++
		if k > 0 {
			odd(k - 1)
		}
	}
	odd = func(k int) {
		if k > 0 {
			even(k - 1)
		}
	}
	even(n)
	across := func() { s += 3 } // acrossFunctions
	viaOther := func() { across() }
	if c {
		across()
	} else {
		viaOther()
	}
	var never func()
	never = func() { s += 4 } // neverRun
	func() { never = nil }()
	_ = &never
	for i := 0; i < n; i++ {
		save := func() { s += i } // perIterationExclusive
		switch k {
		case 0:
			save()
		case 1:
			save()
		default:
			save()
		}
		if c {
			s++
		} else {
			s--
		}
	}
	for i := 0; i < n; i++ {
		save := func() { s -= i } // perIterationTwice
		save()
		save()
	}
	for i := 0; i < n; i++ {
		save := func() { s *= i } // perIterationAcrossBlocks
		if c {
			save()
		}
		if k > 0 {
			save()
		}
	}
	plain := func() { println() } // plainCalledTwice
	plain()
	if c {
		plain()
	}
	var self func()
	self = func() { self() } // selfOnly
	var ping, pong func()
	ping = func() { pong() } // pingPong
	pong = func() { ping() }
	var last func()
	for i := 0; i < n; i++ {
		save := func() { s /= i + 1 } // perIterationKept
		if c {
			last = save
		}
		if last != nil {
			last()
		}
	}
	return s
}`

// repeatTestClosure returns the closure of fn, at any depth, whose literal
// starts on the line that holds marker in src.
func repeatTestClosure(t *testing.T, fn *ssa.Function, src, marker string) *ssa.Function {
	t.Helper()
	line := strings.Count(src[:strings.Index(src, marker)], "\n") + 1
	var found *ssa.Function
	var visit func(f *ssa.Function)
	visit = func(f *ssa.Function) {
		for _, anon := range f.AnonFuncs {
			if fn.Prog.Fset.Position(anon.Pos()).Line == line {
				found = anon
			}
			visit(anon)
		}
	}
	visit(fn)
	if found == nil {
		t.Fatalf("no closure on the line of %q", marker)
	}
	return found
}

// TestRepeatsAfterClosureSites covers a closure called through another
// closure (#305), recursion, and closures made on each iteration (#306).
func TestRepeatsAfterClosureSites(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, repeatClosureSitesSource, "f")
	a := NewAnalyzer()
	tests := []struct {
		marker string
		want   bool
	}{
		{"calledInLoopOfClosure", true},
		{"calledOnceByClosure", false},
		{"calledTwiceInIIFE", true},
		{"recursive", true},
		{"mutual", true},
		// Sites in two functions are taken to both run.
		{"acrossFunctions", true},
		{"neverRun", false},
		{"plainCalledTwice", true},
		{"pingPong", true},
		{"selfOnly", true},
		{"perIterationExclusive", false},
		{"perIterationTwice", true},
		{"perIterationAcrossBlocks", true},
		{"perIterationKept", true},
	}
	for _, tc := range tests {
		closure := repeatTestClosure(t, fn, repeatClosureSitesSource, tc.marker)
		r := a.RepeatsOf(closure)
		// A def where the closure is made: the per-iteration closures share
		// it with their captured values.
		def := a.repeatSitesOf(closure).declared
		if got := r.RepeatsAfter(closure.Blocks[0], def); got != tc.want {
			t.Errorf("%s: RepeatsAfter = %v, want %v", tc.marker, got, tc.want)
		}
		// Asked again, the memoized answer agrees.
		if got := r.RepeatsAfter(closure.Blocks[0], def); got != tc.want {
			t.Errorf("%s, asked again: RepeatsAfter = %v, want %v", tc.marker, got, tc.want)
		}
	}

	// A def in a closure that runs inside another closure at one site is
	// taken at that site, then at the site of that closure: the call of run,
	// before the loops of f.
	inLoop := a.repeatSitesOf(repeatTestClosure(t, fn, repeatClosureSitesSource, "perIterationTwice")).declared
	update := repeatTestClosure(t, fn, repeatClosureSitesSource, "calledInLoopOfClosure")
	if !a.Repeats(fn).RepeatsAfter(inLoop, update.Blocks[0]) {
		t.Error("def in update, use in a later loop: RepeatsAfter = false, want true")
	}
	// Closures that each run only inside the other are lifted to where they
	// are declared on the second visit.
	ping := repeatTestClosure(t, fn, repeatClosureSitesSource, "pingPong")
	if a.Repeats(fn).RepeatsAfter(fn.Blocks[0], ping.Blocks[0]) {
		t.Error("def in ping, use at the entry: RepeatsAfter = true, want false")
	}
}

// TestRepeatsNestedOutside covers a function that r's does not declare: it
// has no Repeats as asked from r, and a site there is taken to run twice.
func TestRepeatsNestedOutside(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, repeatClosureSitesSource, "f")
	other := buildFunc(t, "package p\nfunc g(n int) int { h := func() int { return n }; return h() }", "g")
	a := NewAnalyzer()
	r := a.Repeats(fn)
	if got := r.nested(other); got != nil {
		t.Errorf("nested(g) = %v, want nil", got)
	}
	if got := r.nested(other.AnonFuncs[0]); got != nil {
		t.Errorf("nested(g$1) = %v, want nil", got)
	}
	update := repeatTestClosure(t, fn, repeatClosureSitesSource, "calledOnceByClosure")
	sites := &repeatSites{runs: []ssa.Instruction{other.Blocks[0].Instrs[0]}}
	if !r.repeatRunsMayFollow(update, sites) {
		t.Error("a site in g: repeatRunsMayFollow = false, want true")
	}
}
