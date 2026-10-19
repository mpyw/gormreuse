package fix

import (
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// TestSessionPositionsForRootWithoutPosition checks that a root with no
// source position gets no Session position, instead of a fix at a wrong
// place. Calls through a method value now have their own position (#178); a
// Phi kept as the root of a loop variable swap still has none.
func TestSessionPositionsForRootWithoutPosition(t *testing.T) {
	t.Parallel()

	g := &Generator{fset: token.NewFileSet()}
	root := &ssa.Const{}

	if positions := g.sessionPositionsForRoot(root.Pos(), root, nil); positions != nil {
		t.Errorf("sessionPositionsForRoot() = %v, want nil", positions)
	}
	// A Phi root whose result is used once needs no Session on its edges.
	if positions := g.sessionPositionsForRoot(token.NoPos, &ssa.Phi{}, nil); positions != nil {
		t.Errorf("sessionPositionsForRoot(Phi) = %v, want nil", positions)
	}
}
