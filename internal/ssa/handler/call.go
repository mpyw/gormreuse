// Package handler provides SSA instruction handlers for gormreuse.
//
// # Overview
//
// This package provides handlers for different SSA instruction types that
// can involve *gorm.DB values. Each handler checks for pollution and records
// usage for later violation detection.
//
// # Instruction Types Handled
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│  Instruction Type  │  Handler          │  Purpose                       │
//	├─────────────────────────────────────────────────────────────────────────┤
//	│  *ssa.Call         │  handleSite       │  Method calls, function calls  │
//	│  *ssa.Go           │  handleSite       │  go f(q), go q.Find(nil)       │
//	│  *ssa.Defer        │  handleSite       │  defer f(q), defer q.Find(nil) │
//	│  *ssa.Send         │  sendHandler      │  ch <- db (channel send)       │
//	│  *ssa.Store        │  storeHandler     │  slice[i] = db (slice elem)    │
//	│  *ssa.MapUpdate    │  mapUpdateHandler │  map[k] = db (map storage)     │
//	│  *ssa.MakeInterface│  makeInterfaceHandler │ interface{}(db)            │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// A call, a go statement and a defer statement are one callsite.Site each,
// and handleSite handles all three the same way (see siteUse).
//
// # Type Switch Dispatch
//
// The Dispatch function uses type switch for O(1) dispatch to handlers:
//
//	switch i := instr.(type) {
//	case ssa.CallInstruction: handleSite(ctx.RootTracer.Site(i), ctx)
//	case *ssa.Send:           (&sendHandler{}).handle(i, ctx)
//	...
//	}
//
// This is more efficient than reflection-based dispatch for a small number
// of known types.
package handler

import (
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/effort"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// Context provides shared context for instruction handlers.
//
// This struct is created per-function and passed to all handlers during
// the instruction processing pass.
type Context struct {
	Tracker    *pollution.Tracker // Records usage and detects violations
	RootTracer *tracer.RootTracer // Traces values to mutable roots
	LoopInfo   *cfg.LoopInfo      // Loop detection results for current function
	Repeats    *cfg.Repeats       // Whether a use may run again (current function)
	CurrentFn  *ssa.Function      // The function being analyzed

	// NeedsImmutableParam reports whether a declared function is marked
	// //gormreuse:immutable-param and genuinely relies on immutability (it
	// branches a *gorm.DB parameter). A call passing a mutable *gorm.DB to such
	// a function violates the contract (Phase 1b stage 2b). Functions whose
	// annotation is redundant (never branch a param) are excluded, so they
	// impose no caller-side obligation. A function of another package is
	// decided by its package's fact (#195). It may be nil.
	NeedsImmutableParam func(fn *ssa.Function) bool

	// PosOverride, when valid, replaces the source position recorded for uses
	// inside this function. It is set when analyzing a closure that is invoked
	// at a single known call site, so the closure's captured-value uses are
	// ordered by the call-site (execution) position rather than the closure's
	// (earlier) body position — the define-early/call-late case of #68.
	PosOverride token.Pos

	// assignMerges memoizes, for each call isAssignment walked to the end,
	// the merges its result reaches, and mergeFree the values whose result
	// reaches none (#350). Neither depends on the root asked about.
	assignMerges map[*ssa.Call][]ssa.Value
	mergeFree    map[ssa.Value]bool
}

// pos returns the effective source position to record for a use: the
// PosOverride when set (closure analyzed at its call site), otherwise raw.
func (c *Context) pos(raw token.Pos) token.Pos {
	if c.PosOverride.IsValid() {
		return c.PosOverride
	}
	return raw
}

// isAssignment reports whether call reassigns the variable that holds root.
// That is the case when the call's result, followed forward through chained
// gorm calls and Phi nodes, reaches a merge point — a Phi node or a Store to an
// Alloc — whose possible values include root.
//
// Assignment patterns (don't pollute root):
//   - if c { q = q.Where("x") } → Phi(q, q.Where("x"))
//   - for { q = q.Where() }     → loop-header Phi
//   - q = q.Where("x").Where("y") → chain where final result is assigned
//   - if a { q = q.Where("a"); if b { q = q.Where("b") }; q = q.Where("c") }
//     → q.Where("a") reaches Phi(q, …) through the inner Phi and q.Where("c")
//   - q = helper(q)             → same, for a function returning *gorm.DB
//   - q, err = helper(q)        → same, through the Extract of a tuple result
//   - for { r.db = r.db.Where() } → the field form of the loop rebuild
//     (see isLoopFieldRebuild), also as r.db, err = helper(r.db)
//
// Non-assignment patterns (pollute root):
//   - q.Find(nil) → direct use (finisher)
//   - q.Where("x").Find(nil) → chained use where final result is NOT assigned
//   - if c { q2 = q.Where("a") } → Phi(nil, q.Where("a")): q2 never holds q (#175)
//   - q2 := q.Where("a") captured by a closure → Store to q2's Alloc (#175)
//   - q := db.Where("b"); if c { q = db.Where("a") } → Phi of two roots that
//     are both branches of db, not db itself (#175)
//
// Only a merge that can yield root needs the exemption: a later use of the
// merged value traces back to root, so polluting root here would report that
// use as a false second branch. Any other merge is a different variable, and
// the call is a branch of root like any other.
//
// The merges a call reaches do not depend on root, so a walk that ends without
// finding root keeps them for the call's other roots. A walk that finds no
// merge at all marks every value it went through, and a later walk does not
// go through them again: a chain of n reassignments is walked once, not once
// per call (#350).
func isAssignment(call *ssa.Call, root ssa.Value, ctx *Context) bool {
	if isLoopFieldRebuild(call, ctx) {
		return true
	}
	holdsRoot := func(merge ssa.Value) bool { return ctx.RootTracer.HasMutableRoot(merge, ctx.LoopInfo, root) }
	if merges, ok := ctx.assignMerges[call]; ok {
		return slices.ContainsFunc(merges, holdsRoot)
	}
	if ctx.mergeFree[call] {
		return false
	}
	var merges []ssa.Value
	visited := map[ssa.Value]bool{call: true}
	work := []ssa.Value{call}
	for len(work) > 0 {
		v := work[len(work)-1]
		work = work[:len(work)-1]
		if v != ssa.Value(call) && ctx.mergeFree[v] {
			continue
		}

		for _, user := range *v.Referrers() {
			effort.Step()
			var next, merge ssa.Value
			switch u := user.(type) {
			case *ssa.Phi:
				// Control flow merge: q = ... on one path
				next, merge = u, u
			case *ssa.Extract:
				// The *gorm.DB of a tuple result: q, err = helper(q) (#176)
				if typeutil.IsGormDB(u.Type()) {
					next = u
				}
			case *ssa.Store:
				// A variable kept in memory, e.g. one captured by a closure. A
				// store to a FreeVar is the same assignment made from inside
				// the closure, such as `q = q.Where(v)` in a range-over-func
				// body (#177): the FreeVar's values are those of the captured
				// variable, so it yields root only when the closure updates
				// the variable that holds root.
				switch addr := u.Addr.(type) {
				case *ssa.Alloc, *ssa.FreeVar:
					merge = addr
				}
			case *ssa.Call:
				// Chain intermediate: q.Where("x").Where("y") is an assignment
				// only if Where("y") is
				if isChainedGormMethodCall(v, u) {
					next = u
				}
			}
			if merge != nil {
				merges = append(merges, merge)
				if holdsRoot(merge) {
					return true
				}
			}
			if next != nil && !visited[next] {
				visited[next] = true
				work = append(work, next)
			}
		}
	}

	if ctx.assignMerges == nil {
		ctx.assignMerges = make(map[*ssa.Call][]ssa.Value)
		ctx.mergeFree = make(map[ssa.Value]bool)
	}
	ctx.assignMerges[call] = merges
	if len(merges) == 0 {
		for v := range visited {
			ctx.mergeFree[v] = true
		}
	}
	return false
}

// isLoopFieldRebuild reports whether call, inside a loop, rebuilds a struct
// field or a package-level variable from its own value: its first argument is
// loaded from it, and its result, directly or at the end of a gorm chain, is
// stored back into it.
//
//	for _, c := range conds {
//		r.db = r.db.Where(c)
//	}
//	r.db.Find(nil)
//
// This is the field form of `q = q.Where(c)` in a loop, where the loop-header
// Phi makes the call an assignment. Without it, the load sees the store of the
// previous iteration, so the call is a branch of its own result and the use
// after the loop is reported (#182). A global is loaded and stored the same way
// (#183). Outside a loop the store cannot reach the load, so the call stays a
// branch of the value it read.
func isLoopFieldRebuild(call *ssa.Call, ctx *Context) bool {
	if ctx.LoopInfo == nil || !ctx.LoopInfo.IsInLoop(call.Block()) {
		return false
	}
	site := ctx.RootTracer.Site(call)
	first := site.Recv
	if first == nil && len(site.Args) > 0 {
		first = site.Args[0]
	}
	load, ok := first.(*ssa.UnOp)
	if !ok || load.Op != token.MUL {
		return false
	}
	switch load.X.(type) {
	case *ssa.FieldAddr, *ssa.Global:
		return storesIntoAddr(call, load.X)
	}
	return false
}

// storesIntoAddr reports whether the result v of a call, directly, through
// the *gorm.DB Extract of a tuple result (#176), or at the end of a gorm chain,
// is stored into addr: the same field as a FieldAddr, or the same
// package-level variable (#183).
func storesIntoAddr(v ssa.Value, addr ssa.Value) bool {
	for _, user := range *v.Referrers() {
		switch u := user.(type) {
		case *ssa.Store:
			if u.Val == v && sameAddr(u.Addr, addr) {
				return true
			}
		case *ssa.Extract:
			if typeutil.IsGormDB(u.Type()) && storesIntoAddr(u, addr) {
				return true
			}
		case *ssa.Call:
			if isChainedGormMethodCall(v, u) && storesIntoAddr(u, addr) {
				return true
			}
		}
	}
	return false
}

// sameAddr reports whether a and b address the same field or global.
func sameAddr(a, b ssa.Value) bool {
	faA, okA := a.(*ssa.FieldAddr)
	faB, okB := b.(*ssa.FieldAddr)
	if okA && okB {
		return tracer.SameField(faA, faB)
	}
	return !okA && !okB && a == b
}

// isChainedGormMethodCall checks if nextCall is a gorm method call that uses
// v as receiver (i.e., they form a method chain). A call of a method value
// bound to v is not followed.
func isChainedGormMethodCall(v ssa.Value, nextCall *ssa.Call) bool {
	site := callsite.SiteOfWithoutFuncVars(nextCall)
	return site.IsGormMethod() && site.Recv == v
}

// siteUse is one call, go or defer instruction being handled, with the block
// and the position its uses are recorded at.
//
// Every site is handled the same way, whether it names the function directly
// or through a method value (#254): the receiver of a *gorm.DB method is a
// branch of its roots, an argument passed to a function that is not pure is a
// branch too, and a read-only argument of a *gorm.DB method is a read. The
// mode only decides two things:
//
//   - How an earlier change is found. A plain call checks the roots that
//     reach its block; DetectViolations orders its uses by position later. A
//     go statement checks the uses that reach its block, and a defer checks
//     every use, since it runs when the function returns. go and defer
//     statements are handled after the plain calls of their function, so
//     every use is recorded by then.
//   - What a branch records. A plain call records a use that DetectViolations
//     orders by position. A go or defer records a branch use, which a later
//     go or defer sees but position-ordered detection skips (see
//     pollution.Tracker.branchUses).
type siteUse struct {
	site  callsite.Site
	ctx   *Context
	block *ssa.BasicBlock
	pos   token.Pos
}

// handleSite handles one call, go or defer instruction: its arguments, then
// the receiver of a *gorm.DB method.
func handleSite(site callsite.Site, ctx *Context) {
	u := siteUse{site: site, ctx: ctx, block: site.Instr.Block(), pos: ctx.pos(site.Instr.Pos())}
	u.handleArgs()
	if site.IsGormMethod() {
		u.handleReceiver()
	}
}

// ordered reports whether the site is a plain call, whose uses are ordered by
// position with the other uses.
func (u siteUse) ordered() bool {
	return u.site.Mode == callsite.SiteCall
}

// call returns the site's instruction as a plain call, or nil for a go or
// defer statement. Only a plain call has a result that can reassign a variable.
func (u siteUse) call() *ssa.Call {
	call, _ := u.site.Instr.(*ssa.Call)
	return call
}

// roots returns the mutable roots that v may hold (RootTracer.MutableRoots).
func (u siteUse) roots(v ssa.Value) []ssa.Value {
	return u.ctx.RootTracer.MutableRoots(v, u.ctx.LoopInfo)
}

// checkRoots reports a violation at the site for each of roots that is already
// changed where the site runs. A plain call leaves its first root to
// DetectViolations, which orders the uses by position. Only the first root is
// checked against the branches made through a merge that may hold it
// (RecordMayBranch). It must run before the site's own uses are recorded:
// they would count as the earlier change. A defer runs when the function
// returns, so it checks the uses that may run in the same run of the
// function, before or after it (#375).
func (u siteUse) checkRoots(roots []ssa.Value, use pollution.UsageInfo) {
	t := u.ctx.Tracker
	for i, r := range roots {
		if i == 0 && u.ordered() {
			continue
		}
		if i > 0 {
			use.Direct = false
		}
		var polluted bool
		if u.site.Mode == callsite.SiteDefer {
			polluted = t.IsPollutedAround(r, use) || i == 0 && t.IsMayBranchedAround(r, use)
		} else {
			polluted = t.IsPollutedAt(r, use) || i == 0 && t.IsMayBranchedAt(r, use)
		}
		if polluted {
			t.AddViolationWithRoot(u.pos, r)
		}
	}
}

// read records v as read at the site, without a change: the receiver of a
// method that returns an immutable copy, or a read-only argument (#185). A
// change to one of its roots before the site is reported.
func (u siteUse) read(v ssa.Value, use pollution.UsageInfo) {
	roots := u.roots(v)
	use.Direct = u.direct(v, roots)
	u.checkRoots(roots, use)
	if len(roots) > 0 && u.ordered() {
		u.ctx.Tracker.RecordPureUse(roots[0], use)
	}
}

// branch records the site as a branch of roots, unless assigns reports that
// the call's result reassigns the variable that holds that root (see
// isAssignment). The first root gets the use itself, and the other roots get
// a branch through a merge that may hold them (#252). A branch made on every
// iteration of a loop that a root is defined outside of is reported at once,
// for each of the roots. use.Direct holds for the first root only.
func (u siteUse) branch(roots []ssa.Value, use pollution.UsageInfo, assigns func(root ssa.Value) bool) {
	t := u.ctx.Tracker
	u.checkRoots(roots, use)
	may := use
	may.Direct = false
	for i, r := range roots {
		switch {
		case assigns(r):
			// Assignment creates new root - record but doesn't pollute
			if i == 0 {
				t.RecordAssignment(r, use)
			}
		case i > 0:
			if u.definedBefore(r) {
				t.RecordMayBranch(r, may, u.ordered())
			}
		case !u.ordered():
			t.RecordBranchUse(r, use)
		default:
			t.ProcessBranch(r, use)
		}
	}
	// Every root the site may branch is reused when the site runs again: the
	// first one and those through a merge alike (#306). One diagnostic
	// carries the fixes of them all. A merge may also hold an immutable
	// value, such as a Session result, which FindAllMutableRoots keeps.
	for _, r := range roots {
		if !assigns(r) && !u.ctx.RootTracer.IsImmutableCall(r) && u.ctx.Repeats.RepeatsAfter(u.block, tracer.RootBlock(r)) {
			t.AddViolationWithRoot(u.pos, r)
		}
	}
}

// direct reports whether v, an operand of the site, is roots[0] itself or a
// chain of *gorm.DB method calls on it, each made by name, and the site names
// its function too (pollution.UsageInfo.Direct). A method value, a call
// through a variable and a value that a merge, a variable or a closure holds
// may run on the root of an earlier run of its definition, so none is direct.
func (u siteUse) direct(v ssa.Value, roots []ssa.Value) bool {
	if len(roots) == 0 || u.site.Kind != callsite.SiteStatic {
		return false
	}
	for v != roots[0] {
		call, ok := v.(*ssa.Call)
		if !ok || call.Call.IsInvoke() || len(call.Call.Args) == 0 {
			return false
		}
		callee := call.Call.StaticCallee()
		if callee == nil || callee.Signature.Recv() == nil || !typeutil.IsGormDB(callee.Signature.Recv().Type()) {
			return false
		}
		v = call.Call.Args[0]
	}
	return true
}

// definedBefore reports whether the value that may hold root holds the root
// of this run of the site, not one of an earlier loop iteration. That is so
// when root's block dominates the site's, when root is defined outside every
// loop, or in another function. A root defined in a loop body without
// dominating the site, such as one defined later in the body, may be defined
// again before the later uses read it.
func (u siteUse) definedBefore(root ssa.Value) bool {
	def := tracer.RootBlock(root)
	return def == nil || def.Parent() != u.block.Parent() || def.Dominates(u.block) ||
		u.ctx.LoopInfo == nil || !u.ctx.LoopInfo.IsInLoop(def)
}

// handleReceiver handles the receiver of a *gorm.DB method call, made directly
// or through a method value.
//
// Every gorm chain method use is processed uniformly:
//   - First use from a root: OK, marks root polluted
//   - Second+ use from same root: VIOLATION at that call site
//
// There's no distinction between "terminal" and "non-terminal" methods. A
// method that returns an immutable copy (Session, Debug, ...) reads its
// receiver, a state-only method (AddError, Get, ...) does not use it, and any
// other method branches it, unless its result reassigns the variable
// (isAssignment).
func (u siteUse) handleReceiver() {
	recv, method := u.site.Recv, u.site.Method()
	if typeutil.IsStateOnlyMethod(method) {
		return // touches no clause, so it neither reads nor branches (#307)
	}

	// A receiver that is a Session/Debug copy reads the Statement of the root
	// it was copied from (#174). This holds even though the copy itself is
	// immutable, so it is recorded before its roots are looked up.
	recordCallAliasUses(recv, u.block, u.pos, u.ctx)

	use := pollution.UsageInfo{Block: u.block, Pos: u.pos, Method: method, Call: u.site.Instr.Pos()}
	if typeutil.IsImmutableReturningBuiltin(method) {
		u.read(recv, use)
		return
	}

	call := u.call()
	roots := u.roots(recv)
	use.Direct = u.direct(recv, roots)
	u.branch(roots, use, func(r ssa.Value) bool {
		return call != nil && isAssignment(call, r, u.ctx)
	})
	if call != nil && slices.Contains(roots, ssa.Value(call)) && isLoopFieldRebuild(call, u.ctx) {
		// The call reads its own result from the previous iteration
		// through the field (#182). A use of that result later in the
		// loop body runs before this call on the next iteration.
		u.ctx.Tracker.RecordLoopCarriedUse(call, u.block, u.pos)
	}
}

// handleArgs handles the *gorm.DB arguments of the site.
//
// We conservatively assume non-pure functions may use *gorm.DB arguments.
// Functions marked with //gormreuse:pure are exempt.
//
// Example:
//
//	func helper(db *gorm.DB) { db.Find(nil) }  // might pollute
//	q := db.Where("x")
//	helper(q)      // marks q as polluted
//	q.Count(nil)   // VIOLATION (q already polluted)
//
//	//gormreuse:pure
//	func pureHelper(db *gorm.DB) *gorm.DB { return db }
//	q := db.Where("x")
//	pureHelper(q)  // does NOT pollute
//	q.Count(nil)   // OK (first use)
//
// The receiver of a *gorm.DB method is not among site.Args: handleReceiver
// handles it. Recording it here too gave every use a second copy, and the fix
// generator then counted a reassigning call as a use of the root it creates
// (#202).
func (u siteUse) handleArgs() {
	site, ctx := u.site, u.ctx

	// A Session/Debug copy passed to a function may be read there, pure or not.
	if !site.IsGormMethod() {
		for _, arg := range site.Args {
			if gormArg, ok := pollutionsource.UnwrapGormDB(arg); ok {
				recordCallAliasUses(gormArg, u.block, u.pos, ctx)
			}
		}
	}

	// A *gorm.DB passed to a read-only parameter of a gorm method, such as a
	// subquery, is read, not branched (#185). A change after the call is found
	// from the uses of the call's result, through FindAliasedRoots.
	readOnlyArgs, readStart, hasReadOnly := pollutionsource.CallReadOnlyArgs(site)
	for _, v := range readOnlyArgs {
		recordCallAliasUses(v, u.block, u.pos, ctx)
		u.read(v, pollution.UsageInfo{Block: u.block, Pos: u.pos})
	}

	// Check if this is a pure function - pure functions don't pollute args.
	// Neither does a wrapper such as sql.Named("s", sub) whose result is only
	// a read-only argument: the method that takes it reads sub (#253).
	call := u.call()
	if site.Declared != nil && ctx.RootTracer.IsPureFunction(site.Declared) ||
		call != nil && pollutionsource.IsReadOnlyWrapperCall(call, site) {
		return
	}

	// If the function returns *gorm.DB and result is assigned, treat like gorm method assignment.
	// The assignment creates a new mutable root, so we shouldn't ADD pollution to args.
	// However, we still need to CHECK if args are already polluted (to detect reuse).
	// This enables patterns like: q = buildQuery(q, "filter")
	// A tuple result such as (*gorm.DB, error) counts too: q, err = helper(q).
	returnsGormDB := call != nil && typeutil.HasGormDBResult(call.Type())
	assigns := func(r ssa.Value) bool {
		return returnsGormDB && isAssignment(call, r, ctx)
	}

	for i, arg := range site.Instr.Common().Args {
		p := site.ArgParam(i)
		if p < 0 || hasReadOnly && p >= readStart {
			continue // the receiver, or read above
		}
		// The address of a variable holding a *gorm.DB hands its value on:
		// applyP(&q) may change it in place, as applyTenant(q) may (#345).
		if roots := ctx.RootTracer.PointeeRoots(arg, site.Instr, ctx.LoopInfo); roots != nil {
			u.escape(roots)
			continue
		}
		// A method value such as q.Find hands its receiver on, as a closure
		// that calls q.Find does (#374). So does a function literal that
		// returns a captured *gorm.DB (#397).
		if roots := funcValueRoots(arg, site.Instr, ctx); roots != nil {
			u.escape(roots)
			continue
		}
		// Check if arg is *gorm.DB (directly or wrapped in MakeInterface).
		// Note: We don't skip gorm methods here because we need to pollute
		// *gorm.DB arguments passed through interface{} (e.g., base.Model(q)).
		gormArg, ok := pollutionsource.UnwrapGormDB(arg)
		if !ok {
			continue
		}
		roots := u.roots(gormArg)
		if roots == nil {
			continue
		}

		// Caller-side contract check (Phase 1b stage 2b): passing a mutable
		// *gorm.DB to a parameter of a function that relies on immutability
		// (it branches the parameter) is unsafe: the callee's internal
		// branching interferes because the value is not isolated. It holds
		// for a deferred or spawned call as for a plain one (#192).
		if checksImmutableParamArg(site, i, ctx) {
			ctx.Tracker.AddContractViolation(u.pos, immutableParamContractMessage(ctx.RootTracer.FuncName(site.Callee), ctx.RootTracer.SessionDropsState(gormArg)), site.Instr, p)
		}

		// The function may change the argument in place, so even an
		// assignment escapes it.
		u.branch(roots, pollution.UsageInfo{Block: u.block, Pos: u.pos, Escapes: true, Direct: u.direct(gormArg, roots)}, assigns)
	}

	u.handleHolders()
}

// handleHolders handles the structs that the site hands on, as an argument or
// as the receiver of a method, when they hold a *gorm.DB with a mutable root
// (#341). The callee can reach the root through the struct as well as through
// the *gorm.DB itself, so each such root is branched, as for helper(q):
//
//	q := db.Where("name = ?", "x")
//	useDeps(deps{DB: q})  // a branch of q's root
//	q.Find(&users)        // the second branch
//
// These are the hand-offs that -fix already takes as escapes (#310): a
// struct, its address, or a copy of it passed to a call, also boxed in an
// interface or as the receiver of a method value. A gorm method only reads a
// struct it is passed, and so does a pure function, or a static call to a
// package that does not reach gorm and gets no function. So does a method of
// gorm.Statement, as in `stmt := &gorm.Statement{DB: db}; stmt.Parse(&User{})`:
// it reads only the configuration of stmt.DB, such as its Dialector and
// NamingStrategy, and calls AddError, which changes no clause. So does a getter
// whose call is traced as the field it returns (RootTracer.HasParamReads,
// #342): `r.DB().Find(&a)` is one branch of what r.db holds, at the Find. A
// struct sent on a
// channel, stored in a slice, a map or another variable's memory, or packed
// into a variadic argument is not followed. Neither is a call through an
// interface, whose receiver is boxed before the call.
func (u siteUse) handleHolders() {
	site, ctx := u.site, u.ctx
	if site.IsGormMethod() || isGormStatementMethod(site) {
		return
	}
	if site.Declared != nil && (ctx.RootTracer.IsPureFunction(site.Declared) || ctx.RootTracer.HasParamReads(site.Declared)) {
		return
	}
	operands := site.Args
	if site.Recv != nil {
		operands = append([]ssa.Value{site.Recv}, operands...)
	}
	// Each held value is branched once, with every root it may hold, as an
	// argument is. Two fields that hold one root branch it once.
	var held [][]ssa.Value
	for _, operand := range operands {
		if mi, ok := operand.(*ssa.MakeInterface); ok {
			operand = mi.X
		}
		if typeutil.IsGormDB(operand.Type()) {
			continue // handleArgs or handleReceiver handles it
		}
		for _, v := range ctx.RootTracer.GormDBFieldValues(operand, site.Instr) {
			roots := u.roots(v)
			if roots != nil && !slices.ContainsFunc(held, func(seen []ssa.Value) bool { return seen[0] == roots[0] }) {
				held = append(held, roots)
			}
		}
	}
	if len(held) == 0 || !holderCalleeReachesGorm(site) {
		return
	}
	use := pollution.UsageInfo{Block: u.block, Pos: u.pos, Escapes: true}
	for _, roots := range held {
		u.branch(roots, use, func(ssa.Value) bool { return false })
	}
}

// isGormStatementMethod reports whether site calls a method of gorm.Statement.
func isGormStatementMethod(site callsite.Site) bool {
	if site.Declared == nil {
		return false
	}
	recv := site.Declared.Signature.Recv()
	return recv != nil && typeutil.IsGormStatement(recv.Type())
}

// holderCalleeReachesGorm reports whether the callee of site may reach a
// *gorm.DB inside a struct it is passed. A static call to a package that does
// not reach gorm cannot name the type, unless a function among its arguments
// does it for the callee. Any other call may.
func holderCalleeReachesGorm(site callsite.Site) bool {
	callee := site.Declared
	if callee == nil || callee.Pkg == nil || typeutil.ReachesGorm(callee.Pkg.Pkg) {
		return true
	}
	return slices.ContainsFunc(site.Instr.Common().Args, func(arg ssa.Value) bool {
		_, ok := arg.Type().Underlying().(*types.Signature)
		return ok
	})
}

// recordCallAliasUses records a use, at pos, of every mutable root whose
// Statement v reads: through a Session/Debug copy (#174), or as a read-only
// argument of a call in v's chain (#185). Violations are found later in
// DetectViolations.
func recordCallAliasUses(v ssa.Value, block *ssa.BasicBlock, pos token.Pos, ctx *Context) {
	for _, aliased := range ctx.RootTracer.FindAliasedRoots(v, ctx.LoopInfo) {
		ctx.Tracker.RecordAliasUse(aliased.Root, aliased.Copy.Pos(), aliased.Argument, block, pos)
	}
}

// checksImmutableParamArg reports whether argument i of the call breaks the
// caller-side //gormreuse:immutable-param contract: its callee (or the
// function it stands in for, see origin.Declared) branches its
// parameters, and the argument breaks the contract (see BreaksImmutableParam).
func checksImmutableParamArg(site callsite.Site, i int, ctx *Context) bool {
	if site.Callee == nil || ctx.NeedsImmutableParam == nil || !ctx.NeedsImmutableParam(origin.Declared(site.Callee)) {
		return false
	}
	return BreaksImmutableParam(site, i, ctx.RootTracer, ctx.LoopInfo)
}

// BreaksImmutableParam reports whether argument i of the call at site would
// break the caller-side //gormreuse:immutable-param contract of its callee:
// it is bound to a parameter, not the receiver, which the directive does not
// govern, and its *gorm.DB, directly or in an interface, has a mutable root.
// The fix generator asks it of every argument of every call before it
// declares a function immutable-param, so the declaration cannot make the
// caller-side check report a call (#245).
func BreaksImmutableParam(site callsite.Site, i int, rt *tracer.RootTracer, loopInfo *cfg.LoopInfo) bool {
	if site.ArgParam(i) < 0 {
		return false
	}
	arg, ok := pollutionsource.UnwrapGormDB(site.Instr.Common().Args[i])
	return ok && rt.FindMutableRoot(arg, loopInfo) != nil
}

// immutableParamContractMessage builds the diagnostic for passing a mutable
// *gorm.DB to a parameter of the function named name, which has a
// gormreuse:immutable-param directive. When a Session would drop state the
// argument carries (dropsState, see tracer.RootTracer.SessionDropsState), it
// does not advise one (#344).
func immutableParamContractMessage(name string, dropsState bool) string {
	msg := "mutable *gorm.DB passed to " + name + ", which has a gormreuse:immutable-param directive"
	if dropsState {
		return msg + "; a Session would drop its Attrs, Assign or InstanceSet, so pass a value built for this call instead"
	}
	return msg + "; isolate it with .Session(&gorm.Session{}) before passing"
}

// boundGormReceiver returns the receiver of v when v is a method value of a
// *gorm.DB method, as q.Find is, also converted to a named func type or held
// by a local variable (unwrapFuncValue). A method value boxed in an interface
// is not followed. It is nil otherwise.
func boundGormReceiver(v ssa.Value, ctx *Context) ssa.Value {
	mc, ok := unwrapFuncValue(v, ctx, false).(*ssa.MakeClosure)
	if !ok {
		return nil
	}
	if site, ok := callsite.BoundSite(mc); ok && site.IsGormMethod() {
		return site.Recv
	}
	return nil
}

// unwrapFuncValue returns the function value that v holds: v itself, or what
// a local variable holds (callsite.FuncVars.LocalValue, also through a
// closure that captures it), or what a conversion to a named func type wraps.
// With boxed, it also looks into a box in an interface.
func unwrapFuncValue(v ssa.Value, ctx *Context, boxed bool) ssa.Value {
	for {
		v = ctx.RootTracer.FuncVars().LocalValue(v)
		switch w := v.(type) {
		case *ssa.ChangeType:
			v = w.X
		case *ssa.MakeInterface:
			if !boxed {
				return v
			}
			v = w.X
		default:
			return v
		}
	}
}

// funcValueRoots returns the mutable roots that the function value v, an
// argument of a call, hands on to code that may call it: the receiver of a
// *gorm.DB method value (#374), or the captured values a function literal
// returns (#397). at is the instruction that hands v on. It is nil for any
// other value.
func funcValueRoots(v ssa.Value, at ssa.Instruction, ctx *Context) []ssa.Value {
	if !holdsFunc(v) {
		return nil
	}
	if recv := boundGormReceiver(v, ctx); recv != nil {
		return ctx.RootTracer.MutableRoots(recv, ctx.LoopInfo)
	}
	return returnedCaptureRoots(v, at, ctx)
}

// holdsFunc reports whether a value of v's type may be a function value: a
// func or an interface.
func holdsFunc(v ssa.Value) bool {
	switch v.Type().Underlying().(type) {
	case *types.Signature, *types.Interface:
		return true
	}
	return false
}

// handOn records v, handed on by instr, as a branch of every root it may
// hold: a send, or a store into a slice, a map or memory. Other code may use
// what it was handed, so the hand-off is a branch like a call that takes v
// (#374). It is reported when it runs again, as in a loop, and the roots of a
// merge besides the first get a branch through it.
func handOn(instr ssa.Instruction, v ssa.Value, ctx *Context) {
	u := siteUse{ctx: ctx, block: instr.Block(), pos: ctx.pos(instr.Pos())}
	handOnRoots(instr, u.roots(v), ctx)
}

// handOnRoots records roots, handed on by instr, as branches (see handOn).
func handOnRoots(instr ssa.Instruction, roots []ssa.Value, ctx *Context) {
	if roots == nil {
		return
	}
	siteUse{ctx: ctx, block: instr.Block(), pos: ctx.pos(instr.Pos())}.escape(roots)
}

// escape records roots as branched by code that the site hands them to.
// Other code may change them in place, so none of them is an assignment.
func (u siteUse) escape(roots []ssa.Value) {
	u.branch(roots, pollution.UsageInfo{Block: u.block, Pos: u.pos, Escapes: true}, func(ssa.Value) bool { return false })
}

// handOnFuncValue records the roots that a function value v, sent or stored
// by instr, hands on: the receiver of a method value, as before #397, or the
// captured values of a function literal, when container may leave the
// function (containerEscapes). container is nil for a send, which always
// hands v on.
func handOnFuncValue(instr ssa.Instruction, v, container ssa.Value, ctx *Context) {
	if !holdsFunc(v) {
		return
	}
	if recv := boundGormReceiver(v, ctx); recv != nil {
		handOn(instr, recv, ctx)
		return
	}
	if container != nil && !containerEscapes(container, ctx) {
		return
	}
	handOnRoots(instr, returnedCaptureRoots(v, instr, ctx), ctx)
}

// containerEscapes reports whether a function literal stored into container,
// the address of an element or a field, or a map, may be handed on (#397). It
// may not when the container is a local array, slice, struct or map whose
// only uses build it, read a function value out of it to call it and drop the
// result, or pass it to a read-only stdlib function such as fmt.Println
// (pollutionsource.IsReadOnlyVariadicCall). Any other use, and any other
// container, counts as handing it on.
//
//	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
//	_ = fs // not handed on
func containerEscapes(container ssa.Value, ctx *Context) bool {
	for {
		switch a := container.(type) {
		case *ssa.IndexAddr:
			container = a.X
			continue
		case *ssa.FieldAddr:
			container = a.X
			continue
		}
		break
	}
	switch container.(type) {
	case *ssa.Alloc, *ssa.MakeMap:
	default:
		return true
	}
	vars := ctx.RootTracer.FuncVars()
	work := []ssa.Value{container}
	for len(work) > 0 {
		c := work[len(work)-1]
		work = work[:len(work)-1]
		for _, r := range *c.Referrers() {
			switch r := r.(type) {
			case *ssa.DebugRef:
			case *ssa.IndexAddr, *ssa.FieldAddr, *ssa.Slice:
				work = append(work, r.(ssa.Value))
			case *ssa.Store:
				// A store into it builds it, and a copy into a local variable
				// is the container too.
				if a, local := r.Addr.(*ssa.Alloc); local && r.Val == c {
					work = append(work, a)
				} else if r.Addr != c {
					return true
				}
			case *ssa.MapUpdate:
				if r.Map != c {
					return true
				}
			case *ssa.UnOp, *ssa.Index, *ssa.Field:
				// A load through an address, or an element of a value.
				if containerElemEscapes(r.(ssa.Value), &work) {
					return true
				}
			case *ssa.Lookup:
				if !r.CommaOk {
					if containerElemEscapes(r, &work) {
						return true
					}
					continue
				}
				for _, e := range *r.Referrers() {
					if x, ok := e.(*ssa.Extract); ok && x.Index == 0 && containerElemEscapes(x, &work) {
						return true
					}
				}
			case ssa.CallInstruction:
				if !pollutionsource.IsReadOnlyVariadicCall(r, c, vars) {
					return true
				}
			default:
				return true
			}
		}
	}
	return false
}

// containerElemEscapes reports whether e, a value read from a container, may
// hand a function literal on (see containerEscapes). A struct or an array is
// itself a container, so it goes to work. A function value may only be
// called, with its result dropped. Any other value cannot hold a literal.
func containerElemEscapes(e ssa.Value, work *[]ssa.Value) bool {
	switch e.Type().Underlying().(type) {
	case *types.Struct, *types.Array:
		*work = append(*work, e)
		return false
	case *types.Signature, *types.Interface:
	default:
		return false
	}
	return slices.ContainsFunc(*e.Referrers(), func(r ssa.Instruction) bool {
		call, ok := r.(ssa.CallInstruction)
		if !ok {
			_, debug := r.(*ssa.DebugRef)
			return !debug
		}
		if call.Common().Value != e || slices.Contains(call.Common().Args, e) {
			return true
		}
		// A plain call whose result is used, as in fs[0]().Find(nil).
		result := call.Value()
		return result != nil && slices.ContainsFunc(*result.Referrers(), func(r ssa.Instruction) bool {
			_, debug := r.(*ssa.DebugRef)
			return !debug
		})
	})
}

// returnedCaptureRoots returns the mutable roots of the captured *gorm.DB
// values that v may return, when v is a function literal handed on by at:
// behind any wrapper unwrapFuncValue removes, also a box in an interface, or
// one of several a local variable may hold. The code that gets it may call
// it and chain on what it returns, as Scopes and Preload do, so handing it on
// hands those values on (#397):
//
//	q := db.Where("name = ?", "x")
//	run(func() *gorm.DB { return q }) // a branch of q's root
//	q.Find(&users)                    // the second branch
//
// Only a value the literal returns as it captured it counts (see
// literalReturns). A chain the literal builds is a use in its body already,
// and its own parameter or an immutable value has no root here.
func returnedCaptureRoots(v ssa.Value, at ssa.Instruction, ctx *Context) []ssa.Value {
	s := &literalReturns{ctx: ctx, at: at, seen: map[ssa.Value]bool{}, followed: map[*ssa.MakeClosure]bool{}}
	for _, mc := range handedOnLiterals(v, ctx, map[ssa.Value]bool{}) {
		s.follow(mc, nil, nil)
	}
	return s.roots
}

// handedOnLiterals returns the closures of function literals that v may be:
// v unwrapped (unwrapFuncValue, also a box in an interface), or each edge of
// a merge of such values. A bound method value is not one.
func handedOnLiterals(v ssa.Value, ctx *Context, seen map[ssa.Value]bool) []*ssa.MakeClosure {
	v = unwrapFuncValue(v, ctx, true)
	if seen[v] {
		return nil
	}
	seen[v] = true
	switch w := v.(type) {
	case *ssa.MakeClosure:
		if _, bound := callsite.BoundSite(w); !bound {
			return []*ssa.MakeClosure{w}
		}
	case *ssa.Phi:
		var mcs []*ssa.MakeClosure
		for _, e := range w.Edges {
			mcs = append(mcs, handedOnLiterals(e, ctx, seen)...)
		}
		return mcs
	}
	return nil
}

// literalFrame is a function literal whose returns literalReturns follows.
// outer is the frame of the literal that calls it in place at call. It is nil
// for a literal whose captured variables are read where the literal is
// handed on: the literal handed on, or one it calls through a variable.
type literalFrame struct {
	fn    *ssa.Function
	mc    *ssa.MakeClosure
	outer *literalFrame
	call  ssa.Instruction
}

// literalReturns collects the roots of the captured values that a function
// literal returns as it captured them. It follows merges, a nested literal
// called in place or through a variable, and a load such as a captured
// struct's field. go/ssa captures every variable by reference, so a captured
// value is a load of a FreeVar. It counts when some path reaches the load
// without assigning the variable (unassignedAt): a literal that assigns it
// first returns its own chain, which is a use in its body. A variable that a
// closure, other than one called in place, may assign is left out, since when
// that closure runs is not known.
type literalReturns struct {
	ctx *Context
	// at is the instruction that hands the literal on.
	at ssa.Instruction
	// inside holds the literals followed. A root defined in one of them is
	// the literal's own chain or parameter.
	inside   []*ssa.Function
	followed map[*ssa.MakeClosure]bool
	seen     map[ssa.Value]bool
	roots    []ssa.Value
}

// follow visits the returns of the literal that mc makes, once.
func (s *literalReturns) follow(mc *ssa.MakeClosure, outer *literalFrame, call ssa.Instruction) {
	fn, ok := mc.Fn.(*ssa.Function)
	if !ok || !typeutil.HasGormDBResult(fn.Signature.Results()) || s.followed[mc] {
		return
	}
	s.followed[mc] = true
	s.inside = append(s.inside, fn)
	f := &literalFrame{fn: fn, mc: mc, outer: outer, call: call}
	for _, b := range fn.Blocks {
		ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return)
		if !ok {
			continue
		}
		for _, r := range ret.Results {
			if typeutil.IsGormDB(r.Type()) {
				s.visit(r, f)
			}
		}
	}
}

// visit adds the roots of r, a value of f.fn, that r holds as captured.
func (s *literalReturns) visit(r ssa.Value, f *literalFrame) {
	if s.seen[r] {
		return
	}
	s.seen[r] = true
	switch r := r.(type) {
	case *ssa.Phi:
		for _, e := range r.Edges {
			s.visit(e, f)
		}
	case *ssa.Call:
		// A nested literal called in place returns what it returns. One
		// called through a variable, as a getter chain is, reads its captured
		// variables where the literal is handed on.
		if mc, ok := r.Call.Value.(*ssa.MakeClosure); ok && mc.Parent() == f.fn {
			s.follow(mc, f, r)
			return
		}
		for _, mc := range handedOnLiterals(r.Call.Value, s.ctx, map[ssa.Value]bool{}) {
			s.follow(mc, nil, nil)
		}
	case *ssa.UnOp:
		if fv, ok := r.X.(*ssa.FreeVar); ok && r.Op == token.MUL {
			s.visitCapturedVariable(fv, f, r)
			return
		}
		s.addOutside(s.ctx.RootTracer.MutableRoots(r, nil))
	default:
		s.addOutside(s.ctx.RootTracer.MutableRoots(r, nil))
	}
}

// visitCapturedVariable adds the roots of a variable that f captures by
// reference, loaded at load. It follows the variable out through the frames
// that call f in place, and reads it where the literal is handed on.
func (s *literalReturns) visitCapturedVariable(fv *ssa.FreeVar, f *literalFrame, load ssa.Instruction) {
	at := load
	for {
		if !unassignedAt(fv, at) || assignedByOtherClosure(fv) || branchesCaptured(fv) {
			return
		}
		b := f.mc.Bindings[slices.Index(f.fn.FreeVars, fv)]
		if f.outer == nil {
			s.addPointee(b, f.mc)
			return
		}
		next, ok := b.(*ssa.FreeVar)
		if !ok {
			// A variable of the outer literal's own body, read where the
			// nested literal is made.
			s.addOutside(s.ctx.RootTracer.PointeeRoots(b, f.mc, nil))
			return
		}
		fv, at, f = next, f.call, f.outer
	}
}

// addPointee adds the roots of the variable that ptr, an address that the
// literal mc captures, points to where the literal is handed on (readPoint).
// A variable of a function that encloses mc's function is read where that
// function's closure is made.
func (s *literalReturns) addPointee(ptr ssa.Value, mc *ssa.MakeClosure) {
	at := readPoint(s.at, mc)
	for {
		fv, ok := ptr.(*ssa.FreeVar)
		if !ok {
			break
		}
		// A closure that SSA removed as unreachable has no binding. ptr is
		// then nil, which points to nothing.
		at, ptr = callsite.FreeVarMakeClosure(fv.Parent()), callsite.FreeVarBinding(fv)
	}
	// at is in the function of ptr, a variable there.
	var loopInfo *cfg.LoopInfo
	if alloc, ok := ptr.(*ssa.Alloc); ok && alloc.Parent() == s.ctx.CurrentFn {
		loopInfo = s.ctx.LoopInfo
	}
	s.add(s.ctx.RootTracer.PointeeRoots(ptr, at, loopInfo))
}

// readPoint returns where the variables that the literal mc captures are
// read when it is handed on at at: at itself, or, when at is in a closure
// nested in the function that makes mc, where that closure is made there. So
// an assignment before another closure hands the literal on is seen. A
// literal made where at is not nested is read where it is made.
func readPoint(at ssa.Instruction, mc *ssa.MakeClosure) ssa.Instruction {
	for at.Parent() != mc.Parent() {
		next := callsite.FreeVarMakeClosure(at.Parent())
		if next == nil {
			return mc
		}
		at = next
	}
	return at
}

// unassignedAt reports whether some path from the entry of at's function
// reaches at without assigning the captured variable fv: without a store to
// it, or a call in place of a literal that assigns it on every path.
func unassignedAt(fv *ssa.FreeVar, at ssa.Instruction) bool {
	type span struct {
		b   *ssa.BasicBlock
		end int
	}
	start := at.Block()
	work := []span{{start, slices.Index(start.Instrs, at)}}
	visited := map[*ssa.BasicBlock]bool{}
	for len(work) > 0 {
		sp := work[len(work)-1]
		work = work[:len(work)-1]
		if slices.ContainsFunc(sp.b.Instrs[:sp.end], func(instr ssa.Instruction) bool { return assignsCaptured(instr, fv) }) {
			continue
		}
		if len(sp.b.Preds) == 0 {
			return true
		}
		for _, p := range sp.b.Preds {
			if !visited[p] {
				visited[p] = true
				work = append(work, span{p, len(p.Instrs)})
			}
		}
	}
	return false
}

// assignsCaptured reports whether instr assigns the captured variable fv: a
// store to it, or a call in place of a literal that captures it and assigns
// it on every path to a return. The literal is nested in fv's function, so
// the question goes down a finite nesting.
func assignsCaptured(instr ssa.Instruction, fv *ssa.FreeVar) bool {
	switch i := instr.(type) {
	case *ssa.Store:
		return i.Addr == fv
	case *ssa.Call:
		mc, ok := i.Call.Value.(*ssa.MakeClosure)
		if !ok || !slices.Contains(mc.Bindings, ssa.Value(fv)) {
			return false
		}
		inner := capturedAs(mc, fv)
		return !slices.ContainsFunc(inner.Parent().Blocks, func(b *ssa.BasicBlock) bool {
			ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return)
			return ok && unassignedAt(inner, ret)
		})
	}
	return false
}

// assignedByOtherClosure reports whether a closure that fv's function makes
// may assign the captured variable fv, other than one it only calls in place,
// whose assignments unassignedAt reads. Such a closure may run at any time.
func assignedByOtherClosure(fv *ssa.FreeVar) bool {
	for _, ref := range *fv.Referrers() {
		mc, ok := ref.(*ssa.MakeClosure)
		if !ok {
			continue
		}
		inner := capturedAs(mc, fv)
		if calledInPlace(mc) {
			if assignedByOtherClosure(inner) {
				return true
			}
			continue
		}
		if mayAssignCaptured(inner) {
			return true
		}
	}
	return false
}

// mayAssignCaptured reports whether fv's function, or a closure it makes,
// stores to the captured variable fv.
func mayAssignCaptured(fv *ssa.FreeVar) bool {
	for _, ref := range *fv.Referrers() {
		switch r := ref.(type) {
		case *ssa.Store:
			if r.Addr == fv {
				return true
			}
		case *ssa.MakeClosure:
			if mayAssignCaptured(capturedAs(r, fv)) {
				return true
			}
		}
	}
	return false
}

// capturedAs returns the FreeVar through which the closure that mc makes
// captures v, one of mc's bindings.
func capturedAs(mc *ssa.MakeClosure, v ssa.Value) *ssa.FreeVar {
	return mc.Fn.(*ssa.Function).FreeVars[slices.Index(mc.Bindings, v)]
}

// branchesCaptured reports whether fv's function, or a closure it makes,
// calls a *gorm.DB method that branches the value loaded from the captured
// variable fv. That call is already a use of the value by the literal, so
// handing the literal on adds none. It also stands for a path that returns
// the value unchanged, which is exclusive with it, as in
// `if c { q = q.Where("z") }; return q`.
func branchesCaptured(fv *ssa.FreeVar) bool {
	for _, ref := range *fv.Referrers() {
		switch r := ref.(type) {
		case *ssa.UnOp:
			for _, use := range *r.Referrers() {
				call, ok := use.(ssa.CallInstruction)
				if !ok {
					continue
				}
				site := callsite.SiteOfWithoutFuncVars(call)
				if site.IsGormMethod() && site.Recv == ssa.Value(r) && !typeutil.IsImmutableReturningBuiltin(site.Method()) && !typeutil.IsStateOnlyMethod(site.Method()) {
					return true
				}
			}
		case *ssa.MakeClosure:
			if branchesCaptured(capturedAs(r, fv)) {
				return true
			}
		}
	}
	return false
}

// calledInPlace reports whether every use of mc calls it, as an IIFE does.
func calledInPlace(mc *ssa.MakeClosure) bool {
	return !slices.ContainsFunc(*mc.Referrers(), func(r ssa.Instruction) bool {
		call, ok := r.(*ssa.Call)
		return !ok || call.Call.Value != ssa.Value(mc) || slices.Contains(call.Call.Args, ssa.Value(mc))
	})
}

// addOutside adds the roots that are not defined in a followed literal or in
// a function nested in one. A root inside one is the literal's own chain or
// parameter, which its body uses.
func (s *literalReturns) addOutside(roots []ssa.Value) {
	for _, root := range roots {
		b := tracer.RootBlock(root)
		if b == nil || !slices.ContainsFunc(s.inside, func(fn *ssa.Function) bool { return nestedIn(b.Parent(), fn) }) {
			s.add([]ssa.Value{root})
		}
	}
}

// add adds roots that are not added yet.
func (s *literalReturns) add(roots []ssa.Value) {
	for _, r := range roots {
		if !slices.Contains(s.roots, r) {
			s.roots = append(s.roots, r)
		}
	}
}

// nestedIn reports whether g is fn or a function nested in it.
func nestedIn(g, fn *ssa.Function) bool {
	for ; g != nil; g = g.Parent() {
		if g == fn {
			return true
		}
	}
	return false
}

// handsOnMethodValue reports whether a store into addr hands a method value
// on: any store but one into a local variable, whose calls FuncVars resolves.
func handsOnMethodValue(addr ssa.Value) bool {
	switch addr.(type) {
	case *ssa.Alloc, *ssa.FreeVar:
		return false
	}
	return true
}

// sendHandler handles *ssa.Send instructions.
type sendHandler struct{}

// handle branches a *gorm.DB sent on a channel, also through MakeInterface
// (chan interface{}), and the receiver of a method value sent.
func (h *sendHandler) handle(send *ssa.Send, ctx *Context) {
	if gormVal, kind := pollutionsource.Leak(send, ctx.RootTracer.FuncVars()); kind != pollutionsource.KindNone {
		handOn(send, gormVal, ctx)
	} else {
		handOnFuncValue(send, send.X, nil, ctx)
	}
}

// storeHandler handles *ssa.Store instructions.
type storeHandler struct{}

// handle branches a *gorm.DB stored into a slice or array element, also
// through MakeInterface ([]interface{}), and the receiver of a method value
// stored anywhere but in a local variable.
//
// The read-only variadic stdlib exemption (fmt.Println(q), log.Printf, t.Logf)
// lives in pollutionsource.Leak so the purity validator honors it too.
func (h *storeHandler) handle(store *ssa.Store, ctx *Context) {
	if gormVal, kind := pollutionsource.Leak(store, ctx.RootTracer.FuncVars()); kind != pollutionsource.KindNone {
		handOn(store, gormVal, ctx)
	} else if handsOnMethodValue(store.Addr) {
		handOnFuncValue(store, store.Val, store.Addr, ctx)
	}
}

// mapUpdateHandler handles *ssa.MapUpdate instructions.
type mapUpdateHandler struct{}

// handle branches a *gorm.DB stored in a map, also through MakeInterface
// (map[K]interface{}), and the receiver of a method value stored in one.
func (h *mapUpdateHandler) handle(mapUpdate *ssa.MapUpdate, ctx *Context) {
	if gormVal, kind := pollutionsource.Leak(mapUpdate, ctx.RootTracer.FuncVars()); kind != pollutionsource.KindNone {
		handOn(mapUpdate, gormVal, ctx)
	} else {
		handOnFuncValue(mapUpdate, mapUpdate.Value, mapUpdate.Map, ctx)
	}
}

// makeInterfaceHandler handles *ssa.MakeInterface instructions.
type makeInterfaceHandler struct{}

// handle processes *gorm.DB to interface{} conversion.
// NOTE: Interface conversion itself does NOT pollute the source.
// It's just a type conversion (ownership transfer), similar to assignment.
// The source *gorm.DB is only polluted when actually used via:
// - Type assertion extraction followed by gorm method calls
// - Function calls that receive the interface{} value
// Those are handled by their respective handlers.
func (h *makeInterfaceHandler) handle(mi *ssa.MakeInterface, ctx *Context) {
	// No-op: interface conversion doesn't pollute the source
	// The value is just wrapped in interface{}, not used.
}

// Dispatch routes SSA instructions to their handlers using type switch.
//
// Handler mapping:
//   - *ssa.Call, *ssa.Go, *ssa.Defer → handleSite (calls, goroutines, defers)
//   - *ssa.Send         → sendHandler (channel send: ch <- db)
//   - *ssa.Store        → storeHandler (slice store: slice[i] = db)
//   - *ssa.MapUpdate    → mapUpdateHandler (map store: m[k] = db)
//   - *ssa.MakeInterface → makeInterfaceHandler (interface conversion)
//
// The analyzer passes go and defer statements to DispatchGo and DispatchDefer
// after the other instructions of the function.
func Dispatch(instr ssa.Instruction, ctx *Context) {
	switch i := instr.(type) {
	case ssa.CallInstruction:
		handleSite(ctx.RootTracer.Site(i), ctx)
	case *ssa.Send:
		(&sendHandler{}).handle(i, ctx)
	case *ssa.Store:
		(&storeHandler{}).handle(i, ctx)
	case *ssa.MapUpdate:
		(&mapUpdateHandler{}).handle(i, ctx)
	case *ssa.MakeInterface:
		(&makeInterfaceHandler{}).handle(i, ctx)
	}
}

// DispatchDefer handles defer instructions separately from regular Dispatch.
//
// Defer executes at function exit (not where declared), so it is checked
// against every use in the function, not only the ones that reach it.
//
// Example:
//
//	q := db.Where("x")
//	defer q.Find(nil)  // deferred - executes at function exit
//	q.Count(nil)       // executes BEFORE defer, pollutes q
//	// function exits → defer q.Find(nil) → VIOLATION!
func DispatchDefer(d *ssa.Defer, ctx *Context) {
	handleSite(ctx.RootTracer.Site(d), ctx)
}

// DispatchGo handles go instructions separately from regular Dispatch.
//
// Go statements are processed in a second pass to ensure all pollution
// is recorded first. SSA block order may differ from source order, and
// go statements need to see pollution from all paths before checking.
//
// Example:
//
//	var q *gorm.DB
//	if flag {
//	    q = db.Where("a")
//	} else {
//	    q = db.Where("b")
//	    q.Find(nil)  // pollutes q in this branch
//	}
//	go q.Count(nil)  // needs to see pollution from else branch
func DispatchGo(g *ssa.Go, ctx *Context) {
	handleSite(ctx.RootTracer.Site(g), ctx)
}
