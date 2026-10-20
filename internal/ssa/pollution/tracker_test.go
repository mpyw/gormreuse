package pollution

import (
	"go/constant"
	"go/types"
	"slices"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// TestSortedRootsBreaksTiesByName checks the names sortedRoots falls back to
// when roots share their position and earliest use, as Phi roots without
// uses do. The names are computed once per root (#300), so each root is
// compared more than once here.
func TestSortedRootsBreaksTiesByName(t *testing.T) {
	t.Parallel()
	var want []ssa.Value
	uses := map[ssa.Value][]UsageInfo{}
	for i := range 8 {
		c := ssa.NewConst(constant.MakeInt64(int64(i)), types.Typ[types.Int])
		want = append(want, c)
		uses[c] = nil
	}
	got := sortedRoots(&Tracker{}, uses)
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
