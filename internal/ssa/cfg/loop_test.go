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

	a := NewAnalyzer()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn := buildFunc(t, tc.src, tc.fn)
			info := a.DetectLoops(fn)

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
	if info.IsInLoop(nil) || info.IsLoopHeader(nil) {
		t.Error("empty LoopInfo should report no loop membership")
	}
}

func TestIsDefinedOutsideLoop(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, "package p\nfunc f(n int) int { s := 0; for i := 0; i < n; i++ { s += i }; return s }", "f")
	a := NewAnalyzer()
	info := a.DetectLoops(fn)

	// The entry block, which defines the parameters, is outside any loop. So
	// is a value with no defining block.
	if !info.IsDefinedOutsideLoop(fn.Blocks[0]) {
		t.Error("parameter should be defined outside the loop")
	}
	if !info.IsDefinedOutsideLoop(nil) {
		t.Error("a value without a block should be outside the loop")
	}

	// An instruction inside a loop block is not defined outside the loop.
	var insideFound bool
	for _, b := range fn.Blocks {
		if !info.IsInLoop(b) {
			continue
		}
		for _, instr := range b.Instrs {
			if v, ok := instr.(ssa.Value); ok {
				if info.IsDefinedOutsideLoop(instr.Block()) {
					t.Errorf("value %v in a loop block should be inside the loop", v)
				}
				insideFound = true
			}
		}
	}
	if !insideFound {
		t.Error("expected at least one value defined inside the loop")
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
