package fix

import (
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
)

// mergedEdges returns the other values that the uses of root may read
// instead of it, when they are read twice:
//   - the edges of a Phi that takes root, when the Phi's result is used twice;
//   - for a variable that a closure captures, which SSA keeps in memory with
//     no Phi, the other values stored to it whose store may run before two of
//     uses. The tracer resolves a read of the variable to the first store
//     that reaches it, or that reaches the calls of the closure that reads
//     it, and a read in a closure that may run anywhere to the first store
//     (#251). A store before two of those reads may then add a Session that
//     only another store's reads need, which is redundant but safe.
//
// So in `if c { q = db.Where("a") } else { q = db.Where("b") }` with q read
// twice in a goroutine, both stores get a Session, not only the root's (#247).
//
//declscope:shared // session.go's sessionPositionsForRoot gives each one a Session
func mergedEdges(root ssa.Value, uses []pollution.UsageInfo) []ssa.Value {
	instr, ok := root.(ssa.Instruction)
	if !ok {
		return nil
	}
	fn := instr.Parent()
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if phi, ok := instr.(*ssa.Phi); ok && slices.Contains(phi.Edges, root) {
				if !mergeUsedTwice(phi) {
					return nil
				}
				return phi.Edges
			}
		}
	}

	alloc := mergeVarOf(fn, root)
	if alloc == nil {
		return nil
	}
	var edges []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && store.Addr == alloc && store.Val != root && mergedStoreReadTwice(store, uses) {
				edges = append(edges, store.Val)
			}
		}
	}
	return edges
}

// mergeUsedTwice reports whether the result of phi is used more than once.
//
//declscope:shared // session.go's sessionPositionsForRoot asks it of a Phi root
func mergeUsedTwice(phi *ssa.Phi) bool {
	return len(*phi.Referrers()) > 1
}

// mergeVarOf returns the variable of fn that v is stored to, or nil.
func mergeVarOf(fn *ssa.Function, v ssa.Value) *ssa.Alloc {
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && store.Val == v {
				alloc, _ := store.Addr.(*ssa.Alloc)
				return alloc
			}
		}
	}
	return nil
}

// mergedStoreReadTwice reports whether two of uses may run after store. A use
// in another function, such as a closure, may run after it whatever its block.
func mergedStoreReadTwice(store *ssa.Store, uses []pollution.UsageInfo) bool {
	a := cfg.NewAnalyzer()
	after := 0
	for _, use := range uses {
		switch {
		case use.Block.Parent() != store.Parent():
		case use.Block == store.Block():
			if use.Pos < store.Pos() && !cfg.RepeatsOf(store.Parent()).RunsAgain(use.Block) {
				continue
			}
		case !a.CanReach(store.Block(), use.Block):
			continue
		}
		after++
	}
	return after >= 2
}
