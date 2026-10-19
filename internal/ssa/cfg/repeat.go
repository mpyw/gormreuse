package cfg

import (
	"slices"

	"golang.org/x/tools/go/ssa"
)

// Repeats answers whether a use of a value may run more than once for one
// run of the instruction that defines the value. A use that does reads the
// same value again, so a mutable root used there is reused even with a
// single use in the source.
//
// It is the one place the analyzer asks that question, for the uses the
// checker records and for the suggested fixes. A use runs again when it is in
// a loop that does not define the value, or when its function is a closure
// that may run more than once for one run of the definition (#249):
//
//	q := db.Where("x")
//	f := func() { q.Find(nil) }       // runs twice
//	f(); f()
//	for range ids { f() }             // runs on each iteration
//	for range ids { go q.Find(nil) }  // spawned on each iteration
//	for range ids { g.Go(func() error { return q.Find(nil).Error }) }
//
// So a Repeats holds the loops of its function and, for a closure, the
// Repeats of the function that declares it, with the sites where the closure
// may run there.
type Repeats struct {
	fn     *ssa.Function
	loops  *LoopInfo
	parent *Repeats // the declaring function's, nil when no function declares fn

	// sites are the blocks of parent.fn where the closure fn may run: a
	// call, a go or defer statement, or an instruction that hands the closure
	// to other code, such as an argument or a store.
	sites []*ssa.BasicBlock

	// multi is set when two of the sites may both run on one path, so the
	// closure may run twice for one run of the parent.
	multi bool
}

// NewRepeats returns the Repeats of fn, whose loops are loops, analysed on
// its own: a closure's sites in the function that declares it are not asked
// (see Closure).
func NewRepeats(fn *ssa.Function, loops *LoopInfo) *Repeats {
	return &Repeats{fn: fn, loops: loops}
}

// Closure returns the Repeats of fn, a closure that r's function declares,
// whose loops are loops. It also asks r, at the sites where fn runs.
func (r *Repeats) Closure(fn *ssa.Function, loops *LoopInfo) *Repeats {
	c := &Repeats{fn: fn, loops: loops, parent: r}
	a := NewAnalyzer()
	_, runs := repeatClosureSites(fn)
	for i, site := range runs {
		c.sites = append(c.sites, site.Block())
		for _, other := range runs[:i] {
			if site.Block() == other.Block() || a.CanReach(site.Block(), other.Block()) || a.CanReach(other.Block(), site.Block()) {
				c.multi = true
			}
		}
	}
	return c
}

// RepeatsOf returns the Repeats of fn that asks every function that declares
// it, as the analysis of the outermost one does. The suggested fixes ask it,
// so that they agree with the checker.
func RepeatsOf(fn *ssa.Function) *Repeats {
	loops := NewAnalyzer().DetectLoops(fn)
	if fn.Parent() == nil {
		return NewRepeats(fn, loops)
	}
	return RepeatsOf(fn.Parent()).Closure(fn, loops)
}

// RepeatsAfter reports whether a use in block use, of r's function, may run
// more than once for one run of def, the block that defines the value (see
// tracer.RootBlock). That is so when:
//
//   - use is in a loop that does not hold def. A def in another loop, such as
//     an outer loop or an earlier loop, does not run again with use;
//   - r's function is a closure that runs at two sites that may both run;
//   - a site where the closure runs is in a loop that does not hold def, in
//     the function that declares the closure, or in one that declares that
//     function.
//
// A nil def, such as that of a global, is outside every loop and function. A
// def in a closure that use's function does not run within, such as one an
// IIFE returns, is taken at the site where that closure runs, when it runs at
// one site, or where it is declared. A def in a function that no function of
// use's declares is outside them all.
func (r *Repeats) RepeatsAfter(use, def *ssa.BasicBlock) bool {
	return r.repeatsAfter(use, r.liftRepeatDef(def))
}

// RunsAgain reports whether block, of r's function, may run again after
// itself within one run of the function, as a loop body does. A value
// defined there is defined again, and a use of the previous one, earlier in
// the body, runs after it.
func (r *Repeats) RunsAgain(block *ssa.BasicBlock) bool {
	return r.loops.IsInLoop(block)
}

// repeatsAfter is RepeatsAfter for a def that is in r's function, in one
// that declares it, or nil.
func (r *Repeats) repeatsAfter(use, def *ssa.BasicBlock) bool {
	if r.loops.IsInLoopWithout(use, def) {
		return true
	}
	if def != nil && def.Parent() == r.fn || r.parent == nil {
		return false
	}
	return r.multi || slices.ContainsFunc(r.sites, func(site *ssa.BasicBlock) bool {
		return r.parent.repeatsAfter(site, def)
	})
}

// liftRepeatDef moves def, the block of a definition, to the function of r
// or of one that declares it. A def in another closure is moved to the
// block where the closure runs, or is declared when it does not run at one
// site.
func (r *Repeats) liftRepeatDef(def *ssa.BasicBlock) *ssa.BasicBlock {
	for def != nil {
		for cur := r; cur != nil; cur = cur.parent {
			if cur.fn == def.Parent() {
				return def
			}
		}
		fn := def.Parent()
		if fn.Parent() == nil {
			return nil
		}
		declared, runs := repeatClosureSites(fn)
		def = declared
		if len(runs) == 1 {
			def = runs[0].Block()
		}
	}
	return nil
}

// repeatClosureSites returns the block that declares fn, a closure, in the
// function that declares it, and the instructions there where it may run: a
// call of it, a go or defer statement, or any other use of the closure
// value, such as an argument or a store into a slice. A closure stored in a
// local variable runs where the variable is read. The block is nil when fn
// is not found there.
func repeatClosureSites(fn *ssa.Function) (*ssa.BasicBlock, []ssa.Instruction) {
	var declared *ssa.BasicBlock
	var runs []ssa.Instruction
	for _, block := range fn.Parent().Blocks {
		for _, instr := range block.Instrs {
			if mc, ok := instr.(*ssa.MakeClosure); ok && mc.Fn == fn {
				declared = block
				runs = append(runs, repeatValueSites(mc)...)
				continue
			}
			// A closure with no free variables is a plain function value.
			if slices.ContainsFunc(instr.Operands(nil), func(op *ssa.Value) bool { return *op == fn }) {
				if declared == nil {
					declared = block
				}
				runs = append(runs, instr)
			}
		}
	}
	return declared, runs
}

// repeatValueSites returns the instructions that use v, a closure value. A
// merge (Phi) of it, and a store of it to a local variable, are followed to
// their uses, so a closure assigned in a loop and called after it runs where
// it is called.
func repeatValueSites(v ssa.Value) []ssa.Instruction {
	var sites []ssa.Instruction
	seen := map[ssa.Value]bool{v: true}
	work := []ssa.Value{v}
	for len(work) > 0 {
		v := work[len(work)-1]
		work = work[:len(work)-1]
		follow := func(next ssa.Value) {
			if !seen[next] {
				seen[next] = true
				work = append(work, next)
			}
		}
		for _, ref := range *v.Referrers() {
			switch ref := ref.(type) {
			case *ssa.DebugRef:
				continue
			case *ssa.Phi:
				follow(ref)
				continue
			case *ssa.Store:
				if alloc, ok := ref.Addr.(*ssa.Alloc); ok && ref.Val == v {
					if seen[alloc] {
						continue
					}
					seen[alloc] = true
					// The variable's loads, and the closures that capture it,
					// such as another closure that calls it.
					for _, use := range *alloc.Referrers() {
						switch use := use.(type) {
						case *ssa.DebugRef, *ssa.Store:
						case *ssa.UnOp:
							follow(use)
						default:
							sites = append(sites, use)
						}
					}
					continue
				}
			}
			sites = append(sites, ref)
		}
	}
	return sites
}
