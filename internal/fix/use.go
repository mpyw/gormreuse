package fix

import (
	"cmp"
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// usesOrdered reports whether the fix sees every use of v's root, in the
// function that defines the root, so that findRootsNeedingSession can follow
// their paths. It does not when the root has uses that v.AllUses does not
// list (see pollution.Violation.HiddenUses), when the reported use is not in
// v.AllUses, as for a use through a Phi that the tracker files under another
// of its edges, or when a use is in a closure, such as the body of a
// range-over-func loop.
//
//declscope:shared // generator.go and change.go follow the paths of the uses only when it holds
func usesOrdered(v pollution.Violation) bool {
	def := tracer.RootBlock(v.Root)
	if v.HiddenUses || def == nil {
		return false
	}
	reported := false
	for _, use := range v.AllUses {
		if use.Block.Parent() != def.Parent() {
			return false
		}
		reported = reported || use.Pos == v.Pos
	}
	return reported
}

// runOrderedUses returns uses sorted by position, except that the uses of one
// block are in the order the block runs them. go/ssa puts the post statement
// of a `for` loop at the end of the body's block when nothing else jumps to
// it, so in `for i := 0; i < 2; q.Where("a") { q.Find(nil); i++ }` the post
// statement's call comes first by position but runs after q.Find (#247). The
// uses of each block keep the places that position gives them.
//
//declscope:shared // virtual.go's simulateVirtualRoots takes the uses in this order
func runOrderedUses(uses []pollution.UsageInfo) []pollution.UsageInfo {
	sorted := slices.Clone(uses)
	slices.SortFunc(sorted, func(a, b pollution.UsageInfo) int {
		return cmp.Compare(a.Pos, b.Pos)
	})
	slots := make(map[*ssa.BasicBlock][]int)
	for i, use := range sorted {
		slots[use.Block] = append(slots[use.Block], i)
	}
	ordered := slices.Clone(sorted)
	for block, idx := range slots {
		if len(idx) < 2 {
			continue
		}
		inBlock := make([]pollution.UsageInfo, len(idx))
		for k, i := range idx {
			inBlock[k] = sorted[i]
		}
		slices.SortStableFunc(inBlock, func(a, b pollution.UsageInfo) int {
			return cmp.Compare(useRunIndex(block, a.Pos), useRunIndex(block, b.Pos))
		})
		for k, i := range idx {
			ordered[i] = inBlock[k]
		}
	}
	return ordered
}

// useRunIndex returns the index in block of the instruction at pos, the
// position of a use, or -1 when there is none, as for a use recorded at a
// closure's call site. Such uses keep their position order.
//
//declscope:shared // virtual.go places the reassignments of the fix in their blocks with it
func useRunIndex(block *ssa.BasicBlock, pos token.Pos) int {
	return slices.IndexFunc(block.Instrs, func(instr ssa.Instruction) bool { return instr.Pos() == pos })
}

// usesConflict reports whether two of uses, which are sorted by position,
// may both run on one path: the earlier one can reach the later one, and the
// earlier one is not a reassignment. The uses are in one function.
//
//declscope:shared // session.go's findRootsNeedingSession asks it
func usesConflict(a *cfg.Analyzer, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) bool {
	for i, first := range uses {
		if _, ok := reassigned[first.Pos]; ok {
			continue
		}
		for _, later := range uses[i+1:] {
			if a.CanReach(first.Block, later.Block) {
				return true
			}
		}
	}
	return false
}

// usedAgainInLoop reports whether one of uses, other than a reassignment,
// runs again on the same value of a root defined in block def, the original
// root or a reassignment: the use is in a loop that does not define the root
// again, as in `q := db.Where("x"); for ... { q.Find(nil) }`. A reassignment
// reads the root only on the first iteration; later ones read its result.
// The uses are in def's function.
//
//declscope:shared // session.go's findRootsNeedingSession asks it
func usedAgainInLoop(a *cfg.Analyzer, def *ssa.BasicBlock, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) bool {
	return slices.ContainsFunc(uses, func(use pollution.UsageInfo) bool {
		_, isReassignment := reassigned[use.Pos]
		return !isReassignment && useRepeatsAfter(a, use.Block, def)
	})
}

// useRepeatsAfter reports whether a use in block use may run more than once
// for one run of def, as the checker asks it (cfg.Repeats): a fix and the
// diagnostic it fixes agree on which uses run again.
//
//declscope:shared // change.go and session.go ask the checker's rule with it
func useRepeatsAfter(a *cfg.Analyzer, use, def *ssa.BasicBlock) bool {
	return a.RepeatsOf(use.Parent()).RepeatsAfter(use, def)
}
