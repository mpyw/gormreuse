package directive

import (
	"go/token"
	"testing"

	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"
)

// TestDirectiveFuncSet_Import checks that a function without syntax, which is
// a function of another package, has the directive only when the imported
// fact says so (#195). Its source is never read.
func TestDirectiveFuncSet_Import(t *testing.T) {
	t.Parallel()

	external := &ssa.Function{}
	set := NewPureFuncSet(nil, nil)
	if set.Contains(external) {
		t.Error("Contains = true without an import, want false")
	}
	set.Import(func(fn *ssa.Function) bool { return fn == external })
	if !set.Contains(external) {
		t.Error("Contains = false with an import that has the directive, want true")
	}

	var nilSet *DirectiveFuncSet
	nilSet.Import(func(*ssa.Function) bool { return true })
	if nilSet.Contains(external) {
		t.Error("nil set Contains = true, want false")
	}
}

// TestDirectiveFuncSet_ContainsAfterAdd checks that Contains does not keep a
// miss it remembered once Add puts the function in the set (#301).
func TestDirectiveFuncSet_ContainsAfterAdd(t *testing.T) {
	t.Parallel()

	fn := &ssa.Function{}
	set := NewPureFuncSet(nil, nil)
	if set.Contains(fn) {
		t.Error("Contains = true before Add, want false")
	}
	set.Add(funcKeyOf(fn))
	if !set.Contains(fn) {
		t.Error("Contains = false after Add, want true")
	}
}

// TestDirectiveFuncSet_Nil checks that every method of a nil set, which the
// tracers that resolve builtins only are given, has no directive and does
// not panic (#315).
func TestDirectiveFuncSet_Nil(t *testing.T) {
	t.Parallel()

	fn := &ssa.Function{}
	var set *DirectiveFuncSet
	set.AddFile(inspector.Cursor{})
	set.Import(func(*ssa.Function) bool { return true })
	set.Infer(fn)
	set.Add(funcKeyOf(fn))
	if set.Contains(fn) || set.Inferred(fn) {
		t.Error("nil set has the directive, want none")
	}
	if got := set.CommentsOf(fn); got != nil {
		t.Errorf("nil set CommentsOf = %v, want nil", got)
	}
	if got := set.GetUnusedDirectives(); got != nil {
		t.Errorf("nil set GetUnusedDirectives = %v, want nil", got)
	}
	if set.IsUsed(token.NoPos) {
		t.Error("nil set IsUsed = true, want false")
	}
}
