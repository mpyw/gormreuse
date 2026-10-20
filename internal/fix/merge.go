package fix

import (
	"slices"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// mergedEdges returns the other values that the uses of root may read
// instead of it, when they are read twice:
//   - the values that merge with root through Phis (see phiMergedEdges);
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
//declscope:shared // session.go gives each one a Session
func (g *Generator) mergedEdges(root ssa.Value, uses []pollution.UsageInfo) []ssa.Value {
	// A parameter merges like a local root, as in `if c { db = db.Where("a") }`
	// with db read twice after the if (#311).
	if phis := mergingPhis(root); len(phis) > 0 {
		return g.phiMergedEdges(root, phis)
	}

	fn := root.Parent()
	if fn == nil {
		return nil
	}
	alloc := mergeVarOf(fn, root)
	if alloc == nil {
		return nil
	}
	var edges []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && store.Addr == alloc && store.Val != root && mergedStoreReadTwice(g.cfg, store, uses) {
				edges = append(edges, store.Val)
			}
		}
	}
	return edges
}

// mergedParamFixes returns the fixes of the parameters among the values merged
// with root (see mergedEdges) whose function a fix may rewrite (see
// canFixParam). A parameter has no call to append a Session to. In `if a !=
// "" { db = db.Where("a") }; for ... { db = db.Where(x) }; if b != "" { db =
// db.Where("b") }` the diagnostic's root is the last Where, and the parameter
// needs its own fix in the same pass (#343). Each gets the fix it gets as a
// root: the directive edits, or the site of its body Session. A diagnostic of
// its own in the same pass then agrees with it.
//
//declscope:shared // generator.go's Generate adds them to the root's fix
func (g *Generator) mergedParamFixes(root ssa.Value, uses []pollution.UsageInfo) (edits []analysis.TextEdit, sites []sessionSite) {
	for _, edge := range g.mergedEdges(root, uses) {
		p, ok := edge.(*ssa.Parameter)
		switch {
		case !ok || p == root || !g.canFixParam(p):
		case g.paramCallersIsolate(p):
			edits = append(edits, g.generateImmutableParamFix(p)[0].TextEdits...)
		default:
			sites = append(sites, g.paramSessionSite(p))
		}
	}
	return edits, sites
}

// mergedDropsState reports whether a Session on a value merged with root (see
// mergedEdges) may drop the Attrs, Assign or InstanceSet state of its
// Statement (see RootTracer.SessionDropsState). The walk back through a Phi
// chain reaches values whose chains the root's own check does not see (#343,
// #344).
//
//declscope:shared // generator.go's Generate withholds the Sessions with it
func (g *Generator) mergedDropsState(root ssa.Value, uses []pollution.UsageInfo) bool {
	return slices.ContainsFunc(g.mergedEdges(root, uses), g.tracer.SessionDropsState)
}

// mergingPhis returns root when it is a Phi, and the Phis that take root,
// directly or through another of them. Each conditional filter of
// `db := DB.Model(&User{}); if a != "" { db = db.Where("a") }; if b != "" {
// db = db.Where("b") }` adds one: the second Phi takes the first (#343).
func mergingPhis(root ssa.Value) []*ssa.Phi {
	var phis []*ssa.Phi
	seen := make(map[*ssa.Phi]bool)
	add := func(v ssa.Value) {
		if phi, ok := v.(*ssa.Phi); ok && !seen[phi] {
			seen[phi] = true
			phis = append(phis, phi)
		}
	}
	add(root)
	if refs := root.Referrers(); refs != nil {
		for _, ref := range *refs {
			if v, ok := ref.(ssa.Value); ok {
				add(v)
			}
		}
	}
	for i := 0; i < len(phis); i++ {
		for _, ref := range *phis[i].Referrers() {
			if v, ok := ref.(ssa.Value); ok {
				add(v)
			}
		}
	}
	return phis
}

// phiMergedEdges returns the values that flow into a Phi of phis, those that
// take root (see mergingPhis), whose result is used twice: its edges, and the
// edges of a Phi among them, down to values that are not Phis. Each such value
// is read twice on a path where the Phi takes it, so each needs a Session.
//
// Following only the first Phi, as before #343, made one -fix pass per
// conditional filter. A Session on the first filter's Where made the next
// filter's Where, whose receiver is now immutable, a new mutable root that
// the next Phi merges, and Count's Select leaked into Find until the last
// pass. Following the chain fixes every filter at once. Isolating the last
// Phi instead, with `db = db.Session(&gorm.Session{})` before its first use,
// would need a statement that every use after the merge comes after, which
// SSA's blocks do not give back in source order, and it would add a line
// where the rest of the fix appends to calls.
//
// The checks of the fix (see changedElsewhere and findReassignedChains) see
// the uses of root and of phis, not those of another edge or of a Phi that
// only flows into one of phis, as the loop-header Phi of `for ... { db =
// db.Where(x) }` does when a later filter's Where is the root. Such a value
// is followed only when it is merged or chained on and nothing else (see
// mergeOnly): a Session on what flows into it would make a helper that takes
// it, or a discarded Where on it, change a copy (#243).
func (g *Generator) phiMergedEdges(root ssa.Value, phis []*ssa.Phi) []ssa.Value {
	var edges []ssa.Value
	seen := make(map[ssa.Value]bool)
	forward := make(map[*ssa.Phi]bool, len(phis))
	for _, phi := range phis {
		forward[phi] = true
	}
	var walk func(phi *ssa.Phi)
	walk = func(phi *ssa.Phi) {
		for _, edge := range phi.Edges {
			if seen[edge] {
				continue
			}
			seen[edge] = true
			inner, isPhi := edge.(*ssa.Phi)
			if edge != root && !forward[inner] && !g.mergeOnly(edge) {
				continue
			}
			if isPhi {
				walk(inner)
				continue
			}
			edges = append(edges, edge)
		}
	}
	for _, phi := range phis {
		if !seen[phi] && mergeUsedTwice(phi) {
			seen[phi] = true
			walk(phi)
		}
	}
	return edges
}

// mergeOnly reports whether v is only merged or chained on: each of its
// referrers is a Phi, or a *gorm.DB method with v as its receiver that runs a
// query, returns a copy, or returns a value that is itself only merged or
// chained on. `db = db.Where("a")` in an if is; `applyTenant(db)` and a
// discarded `db.Where("a")` are not.
func (g *Generator) mergeOnly(v ssa.Value) bool {
	refs := v.Referrers()
	if refs == nil || len(*refs) == 0 {
		return false
	}
	for _, ref := range *refs {
		switch ref := ref.(type) {
		case *ssa.Phi:
		case *ssa.Call:
			site := g.tracer.Site(ref)
			if !site.IsGormMethod() || site.Recv != v || slices.Contains(site.Args, v) {
				return false
			}
			if !typeutil.IsFinisher(site.Method()) && !typeutil.IsImmutableReturningBuiltin(site.Method()) && !g.mergeOnly(ref) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// mergeUsedTwice reports whether the result of phi is used more than once.
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
// It stops at the second one.
func mergedStoreReadTwice(a *cfg.Analyzer, store *ssa.Store, uses []pollution.UsageInfo) bool {
	after := 0
	for _, use := range uses {
		switch {
		case use.Block.Parent() != store.Parent():
		case use.Block == store.Block():
			if use.Pos < store.Pos() && !a.RepeatsOf(store.Parent()).RunsAgain(use.Block) {
				continue
			}
		case !a.CanReach(store.Block(), use.Block):
			continue
		}
		if after++; after == 2 {
			return true
		}
	}
	return false
}
