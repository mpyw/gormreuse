package purity_test

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"

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
