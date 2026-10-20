package cfg

import (
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
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
	cfg    *Analyzer // the cache it was made from, which asks the CFG
	loops  *LoopInfo
	parent *Repeats // the declaring function's, nil when no function declares fn

	// sites are where the closure fn may run: a call, a go or defer
	// statement, or an instruction that hands the closure to other code, such
	// as an argument or a store. They are in parent.fn, or in a closure
	// nested in it that calls fn through a variable it captures (#305). They
	// are not kept once multi is set, which answers every question.
	sites []repeatSite

	// multi is set when two of the sites may both run for one run of the
	// MakeClosure, so the closure may run twice for one run of the parent.
	multi bool

	// sitesRepeat memoizes sitesRepeatAfter by def.
	sitesRepeat map[*ssa.BasicBlock]bool
}

// Repeats returns the Repeats of fn analysed on its own: a closure's sites in
// the function that declares it are not asked (see Closure). It is cached.
func (a *Analyzer) Repeats(fn *ssa.Function) *Repeats {
	g := a.graphOf(fn)
	if g.repeats == nil {
		g.repeats = &Repeats{fn: fn, cfg: a, loops: a.DetectLoops(fn)}
	}
	return g.repeats
}

// repeatSite is a block where a closure may run, and the Repeats of the
// function that holds it: the closure's parent, or a closure nested in the
// parent.
type repeatSite struct {
	holder *Repeats
	block  *ssa.BasicBlock
}

// Closure returns the Repeats of fn, a closure that r's function declares. It
// also asks r, at the sites where fn runs. It is cached for each r.
func (r *Repeats) Closure(fn *ssa.Function) *Repeats {
	g := r.cfg.graphOf(fn)
	if c, ok := g.closures[r]; ok {
		return c
	}
	c := &Repeats{fn: fn, cfg: r.cfg, loops: r.cfg.DetectLoops(fn), parent: r}
	// Cached before its sites are found: a closure that runs fn may be run
	// by fn, and asks for it again.
	if g.closures == nil {
		g.closures = make(map[*Repeats]*Repeats)
	}
	g.closures[r] = c
	sites := r.cfg.repeatSitesOf(fn)
	c.multi = r.repeatRunsMayFollow(fn, sites)
	if !c.multi {
		for _, run := range sites.runs {
			c.sites = append(c.sites, repeatSite{holder: r.nested(run.Parent()), block: run.Block()})
		}
	}
	return c
}

// nested returns the Repeats of fn, r's function or a closure nested in it,
// as asked from r. It is nil when fn is not nested in r's function.
func (r *Repeats) nested(fn *ssa.Function) *Repeats {
	switch {
	case fn == r.fn:
		return r
	case fn.Parent() == nil:
		return nil
	}
	outer := r.nested(fn.Parent())
	if outer == nil {
		return nil
	}
	return outer.Closure(fn)
}

// repeatRunsMayFollow reports whether two of the sites where fn, a closure
// that r's function declares, runs may both run for one run of the
// MakeClosure:
//
//   - sites in two functions, such as one in r's function and one in a
//     closure that calls fn through a variable, may (#305);
//   - a site in fn itself, or in a closure nested in fn, runs fn again;
//   - two sites in one function may when one can reach the other. In r's
//     function, a path that runs the MakeClosure again makes a new closure,
//     so `for … { save := func() { … }; if c { save() } else { save() } }`
//     runs each closure once (#306). That holds only when each site uses the
//     MakeClosure itself: a merge or a variable may still hold the closure of
//     an earlier run.
//
// A site in a function that r's does not declare is taken to run twice.
func (r *Repeats) repeatRunsMayFollow(fn *ssa.Function, sites *repeatSites) bool {
	if len(sites.runs) == 0 {
		return false
	}
	in := sites.runs[0].Parent()
	for _, run := range sites.runs {
		if run.Parent() != in {
			return true
		}
	}
	for outer := in; outer != nil; outer = outer.Parent() {
		if outer == fn {
			return true
		}
	}
	if r.nested(in) == nil {
		return true
	}
	var made ssa.Instruction
	if sites.made != nil && in == r.fn && !slices.ContainsFunc(sites.runs, func(run ssa.Instruction) bool {
		return !slices.ContainsFunc(run.Operands(nil), func(op *ssa.Value) bool { return *op == ssa.Value(sites.made) })
	}) {
		made = sites.made
	}
	return repeatRunReachesAnother(sites.runs, made)
}

// repeatRunReachesAnother reports whether one of runs, instructions of one
// function, may run after another one on a path that does not run made (nil
// for none). It labels each block with up to two of the runs whose paths
// reach it, so it takes time linear in the size of the function, not in the
// number of runs times that size.
func repeatRunReachesAnother(runs []ssa.Instruction, made ssa.Instruction) bool {
	// repeatMark is a run, or made (run -1), at index at of its block.
	type repeatMark struct{ run, at int }
	marks := make(map[*ssa.BasicBlock][]repeatMark)
	for i, run := range runs {
		marks[run.Block()] = append(marks[run.Block()], repeatMark{run: i})
	}
	if made != nil {
		marks[made.Block()] = append(marks[made.Block()], repeatMark{run: -1})
	}
	for block, ms := range marks {
		at := make(map[ssa.Instruction]int, len(block.Instrs))
		for i, instr := range block.Instrs {
			at[instr] = i
		}
		for k := range ms {
			if ms[k].run < 0 {
				ms[k].at = at[made]
			} else {
				ms[k].at = at[runs[ms[k].run]]
			}
		}
		slices.SortFunc(ms, func(a, b repeatMark) int { return a.at - b.at })
		// Two runs of one block with no made between them run in turn.
		for k := 1; k < len(ms); k++ {
			if ms[k-1].run >= 0 && ms[k].run >= 0 {
				return true
			}
		}
	}

	// A path leaves a block from its last run, unless made comes after it.
	// It ends at the first block with a mark: another run is reached, and
	// made, or the run the path left from, ends it.
	type repeatPath struct {
		block *ssa.BasicBlock
		run   int
	}
	var work []repeatPath
	leave := func(block *ssa.BasicBlock, run int) {
		for _, succ := range block.Succs {
			work = append(work, repeatPath{succ, run})
		}
	}
	for block, ms := range marks {
		if last := ms[len(ms)-1]; last.run >= 0 {
			leave(block, last.run)
		}
	}
	// labels holds the runs whose paths reach each block, at most two: a
	// third run reaches no block that one of the two does not reach.
	labels := make(map[*ssa.BasicBlock][]int)
	for len(work) > 0 {
		p := work[len(work)-1]
		work = work[:len(work)-1]
		if ms, ok := marks[p.block]; ok {
			if ms[0].run >= 0 && ms[0].run != p.run {
				return true
			}
			continue
		}
		got := labels[p.block]
		if len(got) == 2 || slices.Contains(got, p.run) {
			continue
		}
		labels[p.block] = append(got, p.run)
		leave(p.block, p.run)
	}
	return false
}

// RepeatsOf returns the Repeats of fn that asks every function that declares
// it, as the analysis of the outermost one does. The suggested fixes ask it,
// so that they agree with the checker.
func (a *Analyzer) RepeatsOf(fn *ssa.Function) *Repeats {
	if fn.Parent() == nil {
		return a.Repeats(fn)
	}
	return a.RepeatsOf(fn.Parent()).Closure(fn)
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
	return r.sitesRepeatAfter(def)
}

// sitesRepeatAfter reports whether the closure of r, which a function
// declares, may run more than once for one run of def, a block outside it:
// it runs at two sites that may both run, or a site repeats after def in the
// declaring function. It is memoized by def, so asking it of closures nested
// d deep, each run at k sites, takes k*d steps, not k^d (#299).
func (r *Repeats) sitesRepeatAfter(def *ssa.BasicBlock) bool {
	if r.multi {
		return true
	}
	if got, ok := r.sitesRepeat[def]; ok {
		return got
	}
	if r.sitesRepeat == nil {
		r.sitesRepeat = make(map[*ssa.BasicBlock]bool)
	}
	// Closures that run each other ask for themselves again: they recurse,
	// so they may run again.
	r.sitesRepeat[def] = true
	got := slices.ContainsFunc(r.sites, func(site repeatSite) bool {
		return site.holder.repeatsAfter(site.block, def)
	})
	r.sitesRepeat[def] = got
	return got
}

// liftRepeatDef moves def, the block of a definition, to the function of r
// or of one that declares it. A def in another closure is moved to the
// block where the closure runs, or is declared when it does not run at one
// site.
func (r *Repeats) liftRepeatDef(def *ssa.BasicBlock) *ssa.BasicBlock {
	// lifted holds the closures moved to their site: closures that run each
	// other are moved to where they are declared on the second visit.
	var lifted []*ssa.Function
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
		sites := r.cfg.repeatSitesOf(fn)
		def = sites.declared
		if len(sites.runs) == 1 && !slices.Contains(lifted, fn) {
			lifted = append(lifted, fn)
			def = sites.runs[0].Block()
		}
	}
	return nil
}

// repeatSites are the block that declares a closure, in the function that
// declares it, and the instructions there where it may run (see
// repeatSitesOf).
//
//declscope:shared // analyzer.go's cache holds them
type repeatSites struct {
	//declscope:private
	declared *ssa.BasicBlock
	// made is the MakeClosure, nil for a closure with no free variables.
	//
	//declscope:private
	made *ssa.MakeClosure
	//declscope:private
	runs []ssa.Instruction
}

// repeatSitesOf returns the block that declares fn, a closure, in the
// function that declares it, and the instructions where it may run (see
// callsite.ClosureRuns): a call of it, a go or defer statement, or any other
// use of the closure value, such as an argument or a store into a slice. A
// closure stored in a local variable runs where the variable is read, also in
// a closure that captures the variable (#305). The block is nil when fn is
// not found there. It is cached.
func (a *Analyzer) repeatSitesOf(fn *ssa.Function) *repeatSites {
	g := a.graphOf(fn)
	if g.sites == nil {
		g.sites = findRepeatSites(fn)
	}
	return g.sites
}

// findRepeatSites is repeatSitesOf without the cache. It reads the
// referrers of fn, not every instruction of the function that declares it, so
// finding the sites of n closures in one function is not quadratic in n.
func findRepeatSites(fn *ssa.Function) *repeatSites {
	sites := &repeatSites{}
	var closure ssa.Value = fn
	for _, ref := range *fn.Referrers() {
		if mc, ok := ref.(*ssa.MakeClosure); ok && mc.Fn == fn {
			sites.declared, sites.made, closure = mc.Block(), mc, mc
			break
		}
		// A closure with no free variables is a plain function value,
		// declared at its first use in block order.
		if ref.Parent() == fn.Parent() && (sites.declared == nil || ref.Block().Index < sites.declared.Index) {
			sites.declared = ref.Block()
		}
	}
	for _, run := range callsite.ClosureRuns(closure) {
		sites.runs = append(sites.runs, run.Instr)
	}
	return sites
}
