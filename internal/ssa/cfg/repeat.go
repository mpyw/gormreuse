package cfg

import (
	"go/constant"
	"go/token"
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

	// made is the MakeClosure of fn, nil for a function with no free
	// variables or with no declaring function.
	made *ssa.MakeClosure

	// sitesRepeat memoizes sitesRepeatAfter by def.
	sitesRepeat map[*ssa.BasicBlock]bool

	// exitsRepeat memoizes exitsRepeatAfter.
	exitsRepeat map[repeatExitKey]bool

	// yieldExits memoizes repeatYieldExits by the block of a use.
	yieldExits map[*ssa.BasicBlock]repeatYieldResult
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
	run    ssa.Instruction
	// handedOn is set when run hands the closure on, as a store, an argument
	// or a send does, rather than running it (callsite.ClosureRun.Invoked).
	handedOn bool
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
	c.made = sites.made
	c.multi = r.repeatRunsMayFollow(fn, sites)
	if !c.multi {
		for i, run := range sites.runs {
			// A range-over-func body is handed to its iterator. That call
			// is the loop itself, which exitsRepeatAfter follows.
			handedOn := !sites.invoked[i] && !IsRangeFuncLoopBody(fn)
			c.sites = append(c.sites, repeatSite{holder: r.nested(run.Parent()), block: run.Block(), run: run, handedOn: handedOn})
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
//     an earlier run. Then a site that can reach itself, as `last()` in
//     `for … { save := func() { … }; if c { last = save }; last() }` can,
//     runs that closure again (#309).
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
	kept := false
	if sites.made != nil && in == r.fn {
		if slices.ContainsFunc(sites.runs, func(run ssa.Instruction) bool {
			return !slices.ContainsFunc(run.Operands(nil), func(op *ssa.Value) bool { return *op == ssa.Value(sites.made) })
		}) {
			kept = true
		} else {
			made = sites.made
		}
	}
	return repeatRunReachesAnother(sites.runs, made, kept)
}

// repeatRunReachesAnother reports whether one of runs, instructions of one
// function, may run after another one on a path that does not run made (nil
// for none). With kept, a run that reaches itself counts too: it may run a
// closure that an earlier run ran. It labels each block with up to two of the
// runs whose paths reach it, so it takes time linear in the size of the
// function, not in the number of runs times that size.
func repeatRunReachesAnother(runs []ssa.Instruction, made ssa.Instruction, kept bool) bool {
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
			if ms[0].run >= 0 && (ms[0].run != p.run || kept) {
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
	if !r.multi && len(r.sites) == 1 {
		if exits, ok := r.yieldExitsOf(use); ok {
			return r.useExitsRepeatAfter(def, use, exits)
		}
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
		return site.handedOnInRangeFuncBody(def) || site.holder.repeatsAfter(site.block, def)
	})
	r.sitesRepeat[def] = got
	return got
}

// handedOnInRangeFuncBody reports whether site hands the closure on within a
// range-over-func body that def is outside of: in the body, or in a closure
// that runs within it, at any depth. The closure may then run any number of
// times after the body. A store into a captured variable, as in
// `for range seq { f = func() { … }; break }; f(); f()`, is followed by calls
// that the sites do not see. So such a site counts as running again, even
// when the body leaves the loop after it. That is the answer before #399,
// when every block of the body was in a loop.
//
// The functions from site.holder out to def's are asked in turn. A closure
// on the way that runs nowhere hands nothing on.
func (site repeatSite) handedOnInRangeFuncBody(def *ssa.BasicBlock) bool {
	if !site.handedOn {
		return false
	}
	for h := site.holder; h != nil && (def == nil || h.fn != def.Parent()); h = h.parent {
		if IsRangeFuncLoopBody(h.fn) {
			return true
		}
		if h.parent != nil && !h.multi && len(h.sites) == 0 {
			return false
		}
	}
	return false
}

// repeatExits are the exits that a range-over-func body may take after a use
// (#399). Before each `return false`, go/ssa stores a constant into jump, a
// variable that the code after the loop reads to go on: to the next
// statement, out of an enclosing loop, or out of the function.
type repeatExits struct {
	jump  *ssa.FreeVar
	codes []int64
}

// repeatExitKey memoizes exitsRepeatAfter by def and the block of the use,
// whose exits it follows.
type repeatExitKey struct {
	def, use *ssa.BasicBlock
}

// repeatYieldResult is a memoized result of repeatYieldExits.
type repeatYieldResult struct {
	exits repeatExits
	ok    bool
}

// yieldExitsOf is repeatYieldExits for a use in block use of r's function,
// memoized by the block.
func (r *Repeats) yieldExitsOf(use *ssa.BasicBlock) (repeatExits, bool) {
	if got, ok := r.yieldExits[use]; ok {
		return got.exits, got.ok
	}
	exits, ok := repeatYieldExits(r.fn, use)
	if r.yieldExits == nil {
		r.yieldExits = make(map[*ssa.BasicBlock]repeatYieldResult)
	}
	r.yieldExits[use] = repeatYieldResult{exits, ok}
	return exits, ok
}

// repeatYieldExits returns the exits that fn, a range-over-func body, may take
// from block from. ok is false when fn is no such body, or when an exit is not
// a `return false` in the form go/ssa writes. repeatsAfter asks it only for a
// block outside the body's loop, which reaches no `return true`; such a
// return counts as an exit it cannot read, as a guard.
func repeatYieldExits(fn *ssa.Function, from *ssa.BasicBlock) (exits repeatExits, ok bool) {
	if !IsRangeFuncLoopBody(fn) {
		return repeatExits{}, false
	}
	seen := map[*ssa.BasicBlock]bool{from: true}
	work := []*ssa.BasicBlock{from}
	read := true
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		if ret, isRet := b.Instrs[len(b.Instrs)-1].(*ssa.Return); isRet {
			read = exits.add(ret) && read
		}
		for _, succ := range b.Succs {
			if !seen[succ] {
				seen[succ] = true
				work = append(work, succ)
			}
		}
	}
	slices.Sort(exits.codes)
	return exits, read && exits.jump != nil
}

// add adds the exit that ret takes. It reports false when ret is not a
// `return false` that stores its code as go/ssa writes it, or stores it into
// another variable than the exits before.
func (e *repeatExits) add(ret *ssa.Return) bool {
	jump, code, found := repeatExitCode(ret)
	if !found || e.jump != nil && e.jump != jump {
		return false
	}
	e.jump = jump
	if !slices.Contains(e.codes, code) {
		e.codes = append(e.codes, code)
	}
	return true
}

// repeatExitCode returns the variable and the code that ret's block stores
// just before ret, when ret is a `return false` of a range-over-func body.
func repeatExitCode(ret *ssa.Return) (*ssa.FreeVar, int64, bool) {
	instrs := ret.Block().Instrs
	var store *ssa.Store
	if len(instrs) > 1 {
		store, _ = instrs[len(instrs)-2].(*ssa.Store)
	}
	if loopReturnsTrue(ret.Block()) || store == nil {
		return nil, 0, false
	}
	jump, isFree := store.Addr.(*ssa.FreeVar)
	code, known := repeatConstInt(store.Val)
	return jump, code, isFree && known
}

// repeatConstInt returns the value of v when it is an integer constant. v may
// be any operand of a comparison after the loop, so it may be a nil constant,
// whose Value is nil, or a constant of another kind (#399).
func repeatConstInt(v ssa.Value) (int64, bool) {
	c, ok := v.(*ssa.Const)
	if !ok || c.Value == nil || c.Value.Kind() != constant.Int {
		return 0, false
	}
	return constant.Int64Val(c.Value)
}

// exitsRepeatAfter is sitesRepeatAfter for a use in r's closure, a
// range-over-func body, that leaves the loop by one of exits. The function
// that runs the loop goes on by the exit taken. So the use runs again only
// when that function may run the loop again after one of these exits: a break
// to an outer loop or a return leaves that function's loop too (#399).
func (r *Repeats) exitsRepeatAfter(def *ssa.BasicBlock, exits repeatExits) bool {
	if r.multi || len(r.sites) != 1 || exits.jump == nil {
		return r.sitesRepeatAfter(def)
	}
	site := r.sites[0]
	binding := r.made.Bindings[slices.Index(r.fn.FreeVars, exits.jump)]
	again, outer := repeatExitRunsAgain(site, binding, exits.codes)
	h := site.holder
	var got bool
	switch {
	case def != nil && def.Parent() == h.fn:
		got = again && h.repeatsAfter(site.block, def)
	case again:
		got = true
	case IsRangeFuncLoopBody(h.fn):
		got = h.exitsRepeatAfter(def, outer)
	default:
		got = h.parent != nil && h.sitesRepeatAfter(def)
	}
	return got
}

// useExitsRepeatAfter is exitsRepeatAfter for exits, those of a use in block
// use, memoized by def and use. exitsRepeatAfter asks the functions that hold
// r's closure in turn, outward, so it needs no memo of its own.
func (r *Repeats) useExitsRepeatAfter(def, use *ssa.BasicBlock, exits repeatExits) bool {
	key := repeatExitKey{def: def, use: use}
	if got, ok := r.exitsRepeat[key]; ok {
		return got
	}
	got := r.exitsRepeatAfter(def, exits)
	if r.exitsRepeat == nil {
		r.exitsRepeat = make(map[repeatExitKey]bool)
	}
	r.exitsRepeat[key] = got
	return got
}

// repeatExitRunsAgain follows the function that holds site from site.run,
// the call of an iterator, when the body leaves the loop with one of codes in
// binding, the variable go/ssa reads after the loop. It reports whether the
// function may run site again, or, for a range-over-func body, return true so
// that it runs again. An exit or a condition it cannot read counts as running
// again. Otherwise it returns the exits that a range-over-func body takes.
func repeatExitRunsAgain(site repeatSite, binding ssa.Value, codes []int64) (bool, repeatExits) {
	rangeFunc := IsRangeFuncLoopBody(site.block.Parent())
	start := slices.Index(site.block.Instrs, site.run) + 1
	var exits repeatExits
	for _, code := range codes {
		// The code is read where the call returns. A read in a later block
		// may follow a store, so it is not known.
		vals := map[ssa.Value]int64{}
		conds := map[ssa.Value]bool{}
		seen := map[*ssa.BasicBlock]bool{}
		block, from := site.block, start
		var work []*ssa.BasicBlock
		for {
			var succs []*ssa.BasicBlock
			for _, instr := range block.Instrs[from:] {
				switch instr := instr.(type) {
				case *ssa.UnOp:
					if instr.Op == token.MUL && instr.X == binding && block == site.block {
						vals[instr] = code
					}
				case *ssa.BinOp:
					x, xok := repeatIntOf(instr.X, vals)
					y, yok := repeatIntOf(instr.Y, vals)
					if xok && yok && (instr.Op == token.EQL || instr.Op == token.NEQ) {
						conds[instr] = (x == y) == (instr.Op == token.EQL)
					}
				case *ssa.If:
					succs = block.Succs
					if c, ok := conds[instr.Cond]; ok {
						succs = succs[1:]
						if c {
							succs = block.Succs[:1]
						}
					}
				case *ssa.Jump:
					succs = block.Succs
				case *ssa.Return:
					if rangeFunc && !exits.add(instr) {
						return true, repeatExits{}
					}
				}
			}
			for _, succ := range succs {
				if succ == site.block {
					return true, repeatExits{}
				}
				if !seen[succ] {
					seen[succ] = true
					work = append(work, succ)
				}
			}
			if len(work) == 0 {
				break
			}
			block, from = work[len(work)-1], 0
			work = work[:len(work)-1]
		}
	}
	slices.Sort(exits.codes)
	return false, exits
}

// repeatIntOf returns the integer that v holds: a constant, or a load that
// vals holds.
func repeatIntOf(v ssa.Value, vals map[ssa.Value]int64) (int64, bool) {
	if n, ok := vals[v]; ok {
		return n, true
	}
	return repeatConstInt(v)
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
	// invoked is set for each run that runs the closure itself (see
	// callsite.ClosureRun.Invoked).
	//
	//declscope:private
	invoked []bool
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
		sites.invoked = append(sites.invoked, run.Invoked)
	}
	return sites
}
