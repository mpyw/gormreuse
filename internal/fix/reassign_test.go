package fix

import (
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// TestVarToReassignWithoutPosition checks that a root with no source
// position gets no reassignment target, instead of a fix at a wrong place.
func TestVarToReassignWithoutPosition(t *testing.T) {
	t.Parallel()

	g := &Generator{fset: token.NewFileSet()}
	if v := g.varToReassign(&ssa.Const{}); v != nil {
		t.Errorf("varToReassign() = %v, want nil", v)
	}
}
