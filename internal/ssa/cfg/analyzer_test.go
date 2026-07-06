package cfg

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// buildFunc compiles src (a complete Go source file) to SSA and returns the
// named function. It uses plain Go — the cfg package operates purely on SSA
// control flow, so no gorm types are required.
func buildFunc(t *testing.T, src, name string) *ssa.Function {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkg := types.NewPackage("p", "p")
	ssaPkg, _, err := ssautil.BuildPackage(
		&types.Config{Importer: importer.Default()},
		fset, pkg, []*ast.File{f}, ssa.BuilderMode(0),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	fn := ssaPkg.Func(name)
	if fn == nil {
		t.Fatalf("function %q not found", name)
	}
	return fn
}

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
	}

	a := New()
	for _, tc := range tests {
		tc := tc
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
	info := New().DetectLoops(&ssa.Function{})
	if info == nil {
		t.Fatal("expected non-nil LoopInfo")
	}
	if info.IsInLoop(nil) || info.IsLoopHeader(nil) {
		t.Error("empty LoopInfo should report no loop membership")
	}
}

func TestCanReach(t *testing.T) {
	t.Parallel()
	// entry branches to two exclusive blocks that merge at the return.
	fn := buildFunc(t, "package p\nfunc f(c bool) int {\n\tvar x int\n\tif c {\n\t\tx = 1\n\t} else {\n\t\tx = 2\n\t}\n\treturn x\n}", "f")
	a := New()

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

func TestIsDefinedOutsideLoop(t *testing.T) {
	t.Parallel()
	fn := buildFunc(t, "package p\nfunc f(n int) int { s := 0; for i := 0; i < n; i++ { s += i }; return s }", "f")
	a := New()
	info := a.DetectLoops(fn)

	// A parameter is defined outside any loop.
	if !a.IsDefinedOutsideLoop(fn.Params[0], info) {
		t.Error("parameter should be defined outside the loop")
	}

	// An instruction inside a loop block is not defined outside the loop.
	var insideFound bool
	for _, b := range fn.Blocks {
		if !info.IsInLoop(b) {
			continue
		}
		for _, instr := range b.Instrs {
			if v, ok := instr.(ssa.Value); ok {
				if a.IsDefinedOutsideLoop(v, info) {
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
