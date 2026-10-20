package purity_test

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// TestInferImmutableReturns checks which functions of the fixture
// inferred_immutable_return.go are inferred, and that the result does not
// depend on the order of the candidates (#261).
func TestInferImmutableReturns(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureFuncs(t)
	declared := directive.NewImmutableReturnFuncSet(nil, nil)

	var candidates []*ssa.Function
	for name, fn := range funcs {
		if strings.HasPrefix(name, "inferred") || strings.HasPrefix(name, "notInferred") || strings.HasPrefix(name, "via") ||
			name == "openTestConnection" || name == "setupDB" {
			candidates = append(candidates, fn)
		}
	}
	slices.SortFunc(candidates, func(a, b *ssa.Function) int { return strings.Compare(a.Name(), b.Name()) })

	want := []string{
		"inferredBegin", "inferredBranches", "inferredDebug", "inferredEither", "inferredInner", "inferredMiddle",
		"inferredOuter", "inferredSessionAfterChain", "openTestConnection", "setupDB", "viaBroken", "viaDeclared",
	}
	builtins := tracer.NewRootTracer(nil, nil, nil, nil, nil, nil, nil)
	for i := range candidates {
		// Every rotation, forwards and backwards.
		order := append(slices.Clone(candidates[i:]), candidates[:i]...)
		for _, reversed := range []bool{false, true} {
			if reversed {
				slices.Reverse(order)
			}
			var got []string
			for fn := range purity.InferImmutableReturns(order, declared.Contains, builtins) {
				got = append(got, fn.Name())
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("rotation %d, reversed %v: inferred %v, want %v", i, reversed, got, want)
			}
		}
	}
}

// TestInferImmutableReturnsGrowth checks that inference alone does not grow
// exponentially on a dense chain, where each ci switches over every later cj
// (#298). It counts allocations at k and k+4: before the fix the ratio was
// 2.1, and after it 1.5. The analyzer's count over the whole package hides
// this (1.6 before and after), so TestAllocationGrowth in the root package
// cannot check it. That gate checks the chain a single caller switches over.
// This test does not run in parallel, as testing.AllocsPerRun requires.
func TestInferImmutableReturnsGrowth(t *testing.T) {
	const k = 12
	// Each ci switches over every later cj.
	dense := func(k int) string {
		var b strings.Builder
		for i := range k {
			fmt.Fprintf(&b, "func c%d(db *gorm.DB, x int) *gorm.DB {\n\tswitch x {\n", i)
			for j := i + 1; j <= k; j++ {
				fmt.Fprintf(&b, "\tcase %d:\n\t\treturn c%d(db, x)\n", j, j)
			}
			fmt.Fprintf(&b, "\t}\n\treturn c%d(db, x)\n}\n\n", k)
		}
		fmt.Fprintf(&b, "func c%d(db *gorm.DB, x int) *gorm.DB { return db.Session(&gorm.Session{}) }\n", k)
		return b.String()
	}
	small := immutableReturnInferenceAllocs(t, dense(k))
	large := immutableReturnInferenceAllocs(t, dense(k+4))
	t.Logf("allocations: %.0f at k=%d, %.0f at k=%d", small, k, large, k+4)
	if ratio := large / small; ratio > 2 {
		t.Errorf("allocations grew %.1fx from k=%d to k=%d (%.0f to %.0f), want at most 2x", ratio, k, k+4, small, large)
	}
}

// immutableReturnInferenceAllocs loads src as a package beside the gorm stub,
// checks that inference accepts all of its functions, and returns the
// allocations of one inference run.
func immutableReturnInferenceAllocs(t *testing.T, src string) float64 {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	td := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata")
	gopath := t.TempDir()
	dir := filepath.Join(gopath, "src", "growth")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src = "package growth\n\nimport \"gorm.io/gorm\"\n\n" + src
	if err := os.WriteFile(filepath.Join(dir, "growth.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &packages.Config{
		Mode: packages.LoadAllSyntax,
		Dir:  dir,
		Env:  append(os.Environ(), "GOPATH="+gopath+string(filepath.ListSeparator)+td, "GO111MODULE=off", "GOFLAGS="),
	}
	pkgs, err := packages.Load(cfg, "growth")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("packages had errors")
	}
	prog, ssaPkgs := ssautil.Packages(pkgs, ssa.BuilderMode(0))
	prog.Build()

	var candidates []*ssa.Function
	for _, m := range ssaPkgs[0].Members {
		if fn, ok := m.(*ssa.Function); ok && fn.Name() != "init" {
			candidates = append(candidates, fn)
		}
	}
	// Callers first, as in the shape that grew exponentially.
	slices.SortFunc(candidates, func(a, b *ssa.Function) int { return cmp.Compare(a.Pos(), b.Pos()) })
	declared := directive.NewImmutableReturnFuncSet(nil, nil)
	builtins := tracer.NewRootTracer(nil, nil, nil, nil, nil, nil, nil)
	if got, want := len(purity.InferImmutableReturns(candidates, declared.Contains, builtins)), len(candidates); got != want {
		t.Fatalf("inferred %d functions, want %d", got, want)
	}
	return testing.AllocsPerRun(1, func() {
		purity.InferImmutableReturns(candidates, declared.Contains, builtins)
	})
}
