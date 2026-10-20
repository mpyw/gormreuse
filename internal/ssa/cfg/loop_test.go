package cfg

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestDetectLoops(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		src      string
		fn       string
		wantLoop bool
	}{
		{
			name:     "no loop",
			src:      "package p\nfunc f() int { x := 1; return x }",
			fn:       "f",
			wantLoop: false,
		},
		{
			name:     "if without loop",
			src:      "package p\nfunc f(c bool) int { if c { return 1 }; return 2 }",
			fn:       "f",
			wantLoop: false,
		},
		{
			name:     "counted for loop",
			src:      "package p\nfunc f(n int) int { s := 0; for i := 0; i < n; i++ { s += i }; return s }",
			fn:       "f",
			wantLoop: true,
		},
		{
			name:     "while-style loop",
			src:      "package p\nfunc f(n int) int { for n > 0 { n-- }; return n }",
			fn:       "f",
			wantLoop: true,
		},
		{
			name:     "range loop",
			src:      "package p\nfunc f(xs []int) int { s := 0; for _, x := range xs { s += x }; return s }",
			fn:       "f",
			wantLoop: true,
		},
		{
			// Single-block self-loop: the back-edge source is the header itself
			// (loopTail == loopHead), exercising markLoopBlocks' early return.
			name:     "infinite self-loop",
			src:      "package p\nfunc f() { for {} }",
			fn:       "f",
			wantLoop: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn := buildFunc(t, tc.src, tc.fn)
			// An Analyzer caches, so each parallel subtest has its own.
			info := NewAnalyzer().DetectLoops(fn)

			var headers, blocks int
			for _, b := range fn.Blocks {
				if info.IsLoopHeader(b) {
					headers++
				}
				if info.IsInLoop(b) {
					blocks++
				}
			}
			if tc.wantLoop {
				if headers == 0 {
					t.Errorf("expected a loop header, found none")
				}
				if blocks == 0 {
					t.Errorf("expected in-loop blocks, found none")
				}
			} else {
				if headers != 0 {
					t.Errorf("expected no loop headers, found %d", headers)
				}
				if blocks != 0 {
					t.Errorf("expected no in-loop blocks, found %d", blocks)
				}
			}
		})
	}
}

func TestDetectLoopsNilBlocks(t *testing.T) {
	t.Parallel()
	// An external (bodyless) function has nil Blocks; DetectLoops must not panic.
	info := NewAnalyzer().DetectLoops(&ssa.Function{})
	if info == nil {
		t.Fatal("expected non-nil LoopInfo")
	}
	if info.IsInLoop(nil) || info.IsLoopHeader(nil) || info.HasLoopHeaders() {
		t.Error("empty LoopInfo should report no loop membership")
	}
	var none *LoopInfo
	if none.HasLoopHeaders() {
		t.Error("nil LoopInfo should report no loop header")
	}
	loop := buildFunc(t, `package p
func f(n int) {
	for i := 0; i < n; i++ {
	}
}
`, "f")
	if !NewAnalyzer().DetectLoops(loop).HasLoopHeaders() {
		t.Error("a for loop should have a loop header")
	}
}

// TestIsInLoopWithout guards #252: in nested loops, each loop has its own
// blocks. The outer loop's blocks before the inner loop are in the outer loop,
// and a block of the inner loop is in a loop that a block of the outer loop
// is not in.
func TestIsInLoopWithout(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, `package p
func f(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		if i > 2 {
			s++
		}
		for j := 0; j < n; j++ {
			s += j
		}
	}
	return s
}`, "f")
	info := NewAnalyzer().DetectLoops(fn)

	// The block of s++ is the then-block of `if i > 2`, before the inner
	// loop. It was left out of the outer loop when the loops shared one set.
	var then, inner *ssa.BasicBlock
	for _, b := range fn.Blocks {
		switch b.Comment {
		case "if.then":
			then = b
		case "for.body":
			if inner == nil || b.Index > inner.Index {
				inner = b
			}
		}
	}
	if then == nil || inner == nil {
		t.Fatalf("blocks not found: then=%v inner=%v", then, inner)
	}
	entry := fn.Blocks[0]

	tests := []struct {
		name       string
		block, def *ssa.BasicBlock
		want       bool
	}{
		{"outer body before the inner loop, def before the loops", then, entry, true},
		{"outer body, no def", then, nil, true},
		{"outer body, def in the outer body", then, then, false},
		{"inner body, def in the outer body", inner, then, true},
		{"inner body, def in the inner body", inner, inner, false},
		{"entry", entry, nil, false},
	}
	for _, tc := range tests {
		if got := info.IsInLoopWithout(tc.block, tc.def); got != tc.want {
			t.Errorf("%s: IsInLoopWithout = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMarkLoopBlocksExcludesExit guards #113: a loop nested inside a switch arm,
// followed by post-loop code in that arm, must not have its loop-exit block
// marked in-loop. The invariant is structural — every in-loop block must be able
// to reach a loop header (be genuinely part of the cycle); a mis-marked exit
// block cannot and would fail this.
func TestMarkLoopBlocksExcludesExit(t *testing.T) {
	t.Parallel()
	const src = `package p
func f(s int, xs []int) int {
	r := 0
	switch s {
	case 1:
		r = 1
	case 2:
		for _, x := range xs {
			if x > 0 {
				r += x
			} else {
				r -= x
			}
		}
		r *= 2
	case 3:
		r = 3
	}
	return r
}`
	fn := buildFunc(t, src, "f")
	a := NewAnalyzer()
	info := a.DetectLoops(fn)

	var headers []*ssa.BasicBlock
	for _, b := range fn.Blocks {
		if info.IsLoopHeader(b) {
			headers = append(headers, b)
		}
	}
	if len(headers) == 0 {
		t.Fatal("expected a loop header")
	}
	inLoop := 0
	for _, b := range fn.Blocks {
		if !info.IsInLoop(b) {
			continue
		}
		inLoop++
		reaches := false
		for _, h := range headers {
			if b == h || a.CanReach(b, h) {
				reaches = true
				break
			}
		}
		if !reaches {
			t.Errorf("block #%d marked in-loop but cannot reach any loop header (#113 loop-exit mis-marking)", b.Index)
		}
	}
	if inLoop == 0 {
		t.Fatal("expected in-loop blocks")
	}
}

// TestDetectLoopsForCondKeepsEntryOut guards #252: go/ssa puts the header of
// `for cond { ... }` after its body, so the edge from the header to the body
// is the one found by block index. The walk from that header must not go on
// through the blocks before the loop.
func TestDetectLoopsForCondKeepsEntryOut(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, "package p\nfunc f(n int) int { s := 0; if n > 3 { s = 2 }; for s < n { s++ }; return s }", "f")
	info := NewAnalyzer().DetectLoops(fn)
	for _, b := range fn.Blocks {
		want := b.Comment == "for.body" || b.Comment == "for.loop"
		if got := info.IsInLoop(b); got != want {
			t.Errorf("block %d (%s): IsInLoop = %v, want %v", b.Index, b.Comment, got, want)
		}
	}
}
