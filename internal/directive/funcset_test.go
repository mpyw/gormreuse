package directive

import (
	"testing"

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
