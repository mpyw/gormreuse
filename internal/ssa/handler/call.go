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
	"fmt"
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

	// containers memoizes directContainerHandOffs per container: the
	// element stores of one composite literal all ask about it (#397).
	containers map[ssa.Value]*containerHandOffs

	// captures keeps what returnedCaptureRoots finds about the function's
	// literals (#397).
	captures *captureCache
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
		if roots, literal := funcValueRoots(arg, site.Instr, ctx, nil); roots != nil {
			if literal {
				// A literal's hand-off keeps its own position, not that of
				// the call of the closure it is in (#68), so it never takes
				// the place of a report there.
				lu := u
				lu.pos = site.Instr.Pos()
				lu.escape(roots)
			} else {
				u.escape(roots)
			}
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

// funcValueRoots returns the mutable roots that the function value v hands on
// to code that may call it: the receiver of a *gorm.DB method value (#374),
// or the captured values a function literal returns (#397). at is the
// instruction that hands v on. handsOn, when not nil, decides whether a
// function literal is handed on at all; it is asked only once v is known to
// be one. literal tells which kind the roots come from. The roots are nil for
// any other value.
func funcValueRoots(v ssa.Value, at ssa.Instruction, ctx *Context, handsOn func() bool) (roots []ssa.Value, literal bool) {
	switch v.Type().Underlying().(type) {
	case *types.Signature, *types.Interface:
	default:
		return nil, false
	}
	if recv := boundGormReceiver(v, ctx); recv != nil {
		return ctx.RootTracer.MutableRoots(recv, ctx.LoopInfo), false
	}
	lits := ctx.captureCache().literals(v)
	if len(lits.mcs) == 0 || handsOn != nil && !handsOn() {
		return nil, true
	}
	return returnedCaptureRoots(lits, at, ctx), true
}

// handOn records v, handed on by instr, as a branch of every root it may
// hold: a send, or a store into a slice, a map or memory. Other code may use
// what it was handed, so the hand-off is a branch like a call that takes v
// (#374). It is reported when it runs again, as in a loop, and the roots of a
// merge besides the first get a branch through it.
func handOn(instr ssa.Instruction, v ssa.Value, ctx *Context) {
	handOnRoots(instr, instr.Pos(), ctx.RootTracer.MutableRoots(v, ctx.LoopInfo), ctx)
}

// handOnRoots records roots, handed on by instr at pos, as branches (see
// handOn).
func handOnRoots(instr ssa.Instruction, pos token.Pos, roots []ssa.Value, ctx *Context) {
	if roots == nil {
		return
	}
	siteUse{ctx: ctx, block: instr.Block(), pos: ctx.pos(pos)}.escape(roots)
}

// handOnFuncValue records the roots that the function value v, sent or
// stored by instr, hands on (funcValueRoots). container is what a store or a
// map update writes into, or nil for a send. A function literal stored into
// a container is handed on only as literalStoreHandsOn says. go/ssa gives
// the store of a function literal into the varargs of a call no position, so
// the hand-off is placed at that call (handOffPos).
func handOnFuncValue(instr ssa.Instruction, v, container ssa.Value, ctx *Context) {
	var handsOn func() bool
	if container != nil {
		handsOn = func() bool { return literalStoreHandsOn(instr, container, ctx) }
	}
	roots, literal := funcValueRoots(v, instr, ctx, handsOn)
	if !literal {
		handOnRoots(instr, instr.Pos(), roots, ctx)
		return
	}
	// A literal's hand-off keeps its own position, as in handleArgs.
	if roots != nil {
		siteUse{ctx: ctx, block: instr.Block(), pos: handOffPos(instr)}.escape(roots)
	}
}

// handOffPos returns the position of instr, a store or a send that hands a
// function literal on. go/ssa gives the store of a function literal into the
// varargs array of a call no position, so it is placed at that call.
func handOffPos(instr ssa.Instruction) token.Pos {
	pos := instr.Pos()
	if store, ok := instr.(*ssa.Store); ok && !pos.IsValid() {
		if call := varargsCall(store.Addr); call != nil {
			pos = call.Pos()
		}
	}
	return pos
}

// varargsCall returns the call that passes the array addr is an element of,
// sliced, as its varargs, or nil.
func varargsCall(addr ssa.Value) (call ssa.CallInstruction) {
	if idx, ok := addr.(*ssa.IndexAddr); ok {
		for _, r := range *idx.X.Referrers() {
			if slice, ok := r.(*ssa.Slice); ok {
				for _, use := range *slice.Referrers() {
					if c, ok := use.(ssa.CallInstruction); ok {
						call = c
					}
				}
			}
		}
	}
	return call
}

// escape records roots as branched by code that the site hands them to.
// Other code may change them in place, so none of them is an assignment.
func (u siteUse) escape(roots []ssa.Value) {
	u.branch(roots, pollution.UsageInfo{Block: u.block, Pos: u.pos, Escapes: true}, func(ssa.Value) bool { return false })
}

// literalStoreHandsOn reports whether a function literal stored by store
// into container, the address of an element or a field, or a map, is handed
// on (#397). It is only when the literal goes into a location outside the
// function (externalContainer), or straight into a container the function
// allocates whose own value is handed on after the store
// (containerHandOffs):
//
//	run([]func() *gorm.DB{f})  // handed on
//	m[k] = f                   // handed on, m a parameter
//	fs := make([]func() *gorm.DB, 1)
//	fs[0] = f                  // not followed: a slice of the allocation
//
// Any other shape counts as no hand-off: the literal is followed through no
// load, merge, append, element read, conversion, nesting or capture, which
// keeps a local table of literals from being reported (Known Limitations).
// Main reports no literal hand-off, so leaving a shape out is never a
// regression against it.
func literalStoreHandsOn(store ssa.Instruction, container ssa.Value, ctx *Context) bool {
	base := containerBase(container)
	if externalContainer(base) {
		return true
	}
	switch c := container.(type) {
	case *ssa.IndexAddr:
		if c.X != base {
			return false
		}
	case *ssa.FieldAddr:
		if c.X != base {
			return false
		}
	}
	switch base.(type) {
	case *ssa.Alloc, *ssa.MakeSlice, *ssa.MakeMap:
	default:
		return false
	}
	offs, done := ctx.containers[base]
	if !done {
		offs = directContainerHandOffs(base, ctx)
		if ctx.containers == nil {
			ctx.containers = map[ssa.Value]*containerHandOffs{}
		}
		ctx.containers[base] = offs
	}
	c := ctx.captureCache()
	// A store of the whole variable after the literal's store may replace
	// it, so then the literal is not followed.
	if slices.ContainsFunc(offs.wholes, func(w *ssa.Store) bool { return !c.before(w, store) }) {
		return false
	}
	return slices.ContainsFunc(offs.reads, func(read ssa.Instruction) bool { return c.before(store, read) })
}

// before reports whether a runs before b on every path to b: a's block
// dominates b's, and a comes first in a shared block.
func (c *captureCache) before(a, b ssa.Instruction) bool {
	if a.Block() == b.Block() {
		return c.indexOf(a) < c.indexOf(b)
	}
	return a.Parent() == b.Parent() && a.Block().Dominates(b.Block())
}

// containerHandOffs is where the own value of a container the function
// allocates is handed on: the instruction that reads the container for each
// hand-off, and the stores of the whole variable when the container is one.
type containerHandOffs struct {
	reads  []ssa.Instruction
	wholes []*ssa.Store
}

// containerBase returns the container whose element or field addr is.
func containerBase(addr ssa.Value) ssa.Value {
	for {
		switch a := addr.(type) {
		case *ssa.IndexAddr:
			addr = a.X
		case *ssa.FieldAddr:
			addr = a.X
		default:
			return addr
		}
	}
}

// externalContainer reports whether v is a location outside the function: a
// parameter, a global, or what is read through one. A captured variable is a
// local of the enclosing function, so it is not one.
func externalContainer(v ssa.Value) bool {
	for {
		switch w := v.(type) {
		case *ssa.Parameter, *ssa.Global:
			return true
		case *ssa.UnOp:
			if w.Op != token.MUL {
				return false
			}
			v = w.X
		case *ssa.IndexAddr:
			v = w.X
		case *ssa.FieldAddr:
			v = w.X
		default:
			return false
		}
	}
}

// directContainerHandOffs returns where the value of base, a container the
// function allocates, is handed on as it is: base itself, the slice of it
// that a slice literal or varargs is, or the struct value of a struct
// literal. It is handed on when it is passed to a call (other than a builtin
// or a read-only stdlib function such as fmt.Println), sent, or stored or put
// into a map outside the function. The read of a struct value is its load;
// the read of any other value is the hand-off itself. A container that the
// function returns is not followed, as a literal it returns is not.
func directContainerHandOffs(base ssa.Value, ctx *Context) *containerHandOffs {
	offs := &containerHandOffs{}
	values := []ssa.Value{base}
	for _, r := range *base.Referrers() {
		switch r := r.(type) {
		case *ssa.Slice:
			if r.X == base {
				values = append(values, r)
			}
		case *ssa.UnOp:
			if r.Op == token.MUL {
				values = append(values, r)
			}
		case *ssa.Store:
			if r.Addr == base {
				offs.wholes = append(offs.wholes, r)
			}
		}
	}
	vars := ctx.RootTracer.FuncVars()
	for _, v := range values {
		for _, r := range *v.Referrers() {
			if !containerHandsOn(v, r, vars) {
				continue
			}
			read := r
			if load, ok := v.(*ssa.UnOp); ok {
				read = load
			}
			offs.reads = append(offs.reads, read)
		}
	}
	return offs
}

// containerHandsOn reports whether r hands v, the own value of a container,
// on (see directContainerHandOffs).
func containerHandsOn(v ssa.Value, r ssa.Instruction, vars callsite.FuncVars) bool {
	switch r := r.(type) {
	case ssa.CallInstruction:
		if _, builtin := r.Common().Value.(*ssa.Builtin); builtin {
			return false
		}
		return slices.Contains(r.Common().Args, v) && !pollutionsource.IsReadOnlyVariadicCall(r, v, vars)
	case *ssa.Send:
		return r.X == v
	case *ssa.Store:
		return r.Val == v && externalContainer(containerBase(r.Addr))
	case *ssa.MapUpdate:
		return r.Value == v && externalContainer(r.Map)
	}
	return false
}

// returnedCaptureRoots returns the mutable roots of the captured *gorm.DB
// values that the function literals mcs may return, handed on by at. mcs are
// the literals a function value may be (handedOnLiterals). The code that gets
// it may call it and chain on what it returns, as Scopes and Preload do, so
// handing it on hands those values on (#397):
//
//	q := db.Where("name = ?", "x")
//	run(func() *gorm.DB { return q }) // a branch of q's root
//	q.Find(&users)                    // the second branch
//
// Only a value the literal returns as it captured it counts (see
// literalSummary). A chain the literal builds is a use in its body already,
// and its own parameter or an immutable value has no root here. What a
// literal returns, apart from where it is handed on, is kept per literal;
// only the reads of its captured variables depend on at.
func returnedCaptureRoots(lits *handedLiterals, at ssa.Instruction, ctx *Context) []ssa.Value {
	c := ctx.captureCache()
	sum := c.merged(lits)
	roots := slices.Clone(sum.roots)
	for _, read := range sum.reads {
		for _, r := range c.read(read, at) {
			if !slices.Contains(roots, r) {
				roots = append(roots, r)
			}
		}
	}
	return roots
}

// handedLiterals is the function literals that a function value may be
// (handedOnLiterals), and, once asked, the union of their summaries.
type handedLiterals struct {
	mcs []*ssa.MakeClosure
	sum *literalSummary
}

// literals returns the handedLiterals of v, kept per value: a variable that
// holds one of n literals may be handed on n times.
func (c *captureCache) literals(v ssa.Value) *handedLiterals {
	v = unwrapFuncValue(v, c.ctx, true)
	if lits, done := c.values[v]; done {
		return lits
	}
	lits := &handedLiterals{mcs: handedOnLiterals(v, c.ctx, map[ssa.Value]bool{})}
	c.values[v] = lits
	return lits
}

// merged returns the union of the summaries of lits. Reads of one variable
// by literals made in one function are read at the same point, so one of
// them stands for all.
func (c *captureCache) merged(lits *handedLiterals) *literalSummary {
	if lits.sum != nil {
		return lits.sum
	}
	sum := &literalSummary{}
	for _, mc := range lits.mcs {
		part := c.summary(mc)
		for _, r := range part.roots {
			if !slices.Contains(sum.roots, r) {
				sum.roots = append(sum.roots, r)
			}
		}
		for _, read := range part.reads {
			if !slices.ContainsFunc(sum.reads, func(r capturedRead) bool {
				return r.ptr == read.ptr && r.key == read.key && r.mc.Parent() == read.mc.Parent()
			}) {
				sum.reads = append(sum.reads, read)
			}
		}
	}
	lits.sum = sum
	return sum
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

// captureCache keeps what returnedCaptureRoots asks about the literals of
// the function being analyzed (#397). The summary of a literal and the
// answers about a captured variable do not depend on where the literal is
// handed on, so each is found once. A read of a variable depends on its read
// point, and is kept per read point.
type captureCache struct {
	ctx       *Context
	summaries map[*ssa.MakeClosure]*literalSummary
	flows     map[*ssa.FreeVar]*assignFlow
	index     map[ssa.Instruction]int
	others    map[*ssa.FreeVar]bool
	branches  map[*ssa.FreeVar]bool
	writes    map[capturedWriteKey]bool
	calls     map[*ssa.MakeClosure][]*ssa.Call
	reads     map[capturedReadKey][]ssa.Value
	values    map[ssa.Value]*handedLiterals
	single    map[*ssa.Alloc]*ssa.Store
}

// captureCache returns the cache of the function being analyzed.
func (c *Context) captureCache() *captureCache {
	if c.captures == nil {
		c.captures = &captureCache{
			ctx:       c,
			summaries: map[*ssa.MakeClosure]*literalSummary{},
			flows:     map[*ssa.FreeVar]*assignFlow{},
			index:     map[ssa.Instruction]int{},
			others:    map[*ssa.FreeVar]bool{},
			branches:  map[*ssa.FreeVar]bool{},
			writes:    map[capturedWriteKey]bool{},
			calls:     map[*ssa.MakeClosure][]*ssa.Call{},
			reads:     map[capturedReadKey][]ssa.Value{},
			values:    map[ssa.Value]*handedLiterals{},
			single:    map[*ssa.Alloc]*ssa.Store{},
		}
	}
	return c.captures
}

// literalSummary is what a function literal may return of what it captured,
// apart from where it is handed on: the roots it returns from outside it,
// and the captured variables it returns as they are where it is handed on.
type literalSummary struct {
	roots []ssa.Value
	reads []capturedRead
}

// capturedRead is a captured variable, or a field path of one, that a
// literal returns: ptr is the variable's address in the function that makes
// mc, and path the field path, with key its string form.
type capturedRead struct {
	ptr  ssa.Value
	path []int
	key  string
	mc   *ssa.MakeClosure
}

// capturedReadKey is a variable, a field path of it, and where it is read.
type capturedReadKey struct {
	ptr  ssa.Value
	path string
	at   ssa.Instruction
}

// capturedWriteKey is a captured variable, and whether stores into its fields
// count (see writesCaptured).
type capturedWriteKey struct {
	fv     *ssa.FreeVar
	fields bool
}

// summary returns the summary of the literal that mc makes. A literal's
// summary holds what it returns itself (summaryBuilder) and the summaries of
// the literals it calls through a variable. Literals that call each other,
// as mutually recursive getters do, share one summary: the summaries are
// found per strongly connected component of those calls (summarySCC), so
// each literal is walked once.
func (c *captureCache) summary(mc *ssa.MakeClosure) *literalSummary {
	if sum, done := c.summaries[mc]; done {
		return sum
	}
	(&summarySCC{c: c, index: map[*ssa.MakeClosure]int{}, low: map[*ssa.MakeClosure]int{}, onStack: map[*ssa.MakeClosure]bool{}, own: map[*ssa.MakeClosure]*summaryBuilder{}}).visit(mc)
	return c.summaries[mc]
}

// summarySCC finds summaries with Tarjan's algorithm over the calls of
// literals through variables.
type summarySCC struct {
	c       *captureCache
	index   map[*ssa.MakeClosure]int
	low     map[*ssa.MakeClosure]int
	onStack map[*ssa.MakeClosure]bool
	stack   []*ssa.MakeClosure
	own     map[*ssa.MakeClosure]*summaryBuilder
}

// visit walks mc and the literals it calls, and keeps the summary of each
// component once it is complete.
func (s *summarySCC) visit(mc *ssa.MakeClosure) {
	s.index[mc], s.low[mc] = len(s.index), len(s.index)
	s.stack = append(s.stack, mc)
	s.onStack[mc] = true
	b := &summaryBuilder{c: s.c, followed: map[*ssa.MakeClosure]bool{}, seen: map[ssa.Value]bool{}}
	b.follow(mc, nil, nil)
	s.own[mc] = b
	for _, w := range b.calls {
		if _, done := s.c.summaries[w]; done {
			continue
		}
		if _, seen := s.index[w]; !seen {
			s.visit(w)
			s.low[mc] = min(s.low[mc], s.low[w])
		} else if s.onStack[w] {
			s.low[mc] = min(s.low[mc], s.index[w])
		}
	}
	if s.low[mc] != s.index[mc] {
		return
	}
	// mc is the root of a component: its members are on the stack above it.
	i := slices.Index(s.stack, mc)
	members := s.stack[i:]
	s.stack = s.stack[:i]
	union := &summaryBuilder{c: s.c}
	for _, m := range members {
		s.onStack[m] = false
		union.inside = append(union.inside, s.own[m].inside...)
	}
	for _, m := range members {
		own := s.own[m]
		union.addOutside(own.sum.roots)
		for _, read := range own.sum.reads {
			union.addRead(read)
		}
		for _, w := range own.calls {
			if sum, done := s.c.summaries[w]; done {
				union.addOutside(sum.roots)
				for _, read := range sum.reads {
					union.addRead(read)
				}
			}
		}
	}
	for _, m := range members {
		s.c.summaries[m] = &union.sum
	}
}

// read returns the roots of what read holds where a literal is handed on at
// handOff (readPoint). A variable of a function that encloses the function
// that makes the literal is read where that function's closure is made.
func (c *captureCache) read(read capturedRead, handOff ssa.Instruction) []ssa.Value {
	at, ptr := c.readPoint(handOff, read.mc), read.ptr
	for {
		fv, ok := ptr.(*ssa.FreeVar)
		if !ok {
			break
		}
		// A closure that SSA removed as unreachable has no binding. ptr is
		// then nil, which points to nothing.
		at, ptr = callsite.FreeVarMakeClosure(fv.Parent()), callsite.FreeVarBinding(fv)
	}
	key := capturedReadKey{ptr, read.key, at}
	if alloc, ok := ptr.(*ssa.Alloc); ok && len(read.path) == 0 {
		// A variable that one store assigns, and nothing else changes,
		// holds the stored value at every read the store dominates.
		if store := c.onlyStore(alloc); store != nil && c.dominates(store, at) {
			key.at = store
		}
	}
	if roots, done := c.reads[key]; done {
		return roots
	}
	// at is in the function of ptr, a variable there.
	var loopInfo *cfg.LoopInfo
	if alloc, ok := ptr.(*ssa.Alloc); ok && alloc.Parent() == c.ctx.CurrentFn {
		loopInfo = c.ctx.LoopInfo
	}
	roots := readVariable(c.ctx, ptr, read.path, at, loopInfo)
	c.reads[key] = roots
	return roots
}

// onlyStore returns the one store into alloc when nothing else may change
// it: the variable is only loaded, and the closures that capture it only
// load it. It is nil otherwise.
func (c *captureCache) onlyStore(alloc *ssa.Alloc) *ssa.Store {
	if store, done := c.single[alloc]; done {
		return store
	}
	var only *ssa.Store
	var onlyLoaded func(addr ssa.Value) bool
	onlyLoaded = func(addr ssa.Value) bool {
		return !slices.ContainsFunc(*addr.Referrers(), func(r ssa.Instruction) bool {
			switch r := r.(type) {
			case *ssa.UnOp, *ssa.DebugRef:
				return false
			case *ssa.Store:
				if r.Addr == addr && addr == ssa.Value(alloc) && only == nil {
					only = r
					return false
				}
			case *ssa.MakeClosure:
				return !onlyLoaded(capturedAs(r, addr))
			}
			return true
		})
	}
	if !onlyLoaded(alloc) {
		only = nil
	}
	c.single[alloc] = only
	return only
}

// dominates reports whether store runs before at on every path to it: its
// block dominates at's block, and it comes first in a shared block.
func (c *captureCache) dominates(store *ssa.Store, at ssa.Instruction) bool {
	if store.Block() == at.Block() {
		return c.indexOf(store) < c.indexOf(at)
	}
	return store.Parent() == at.Parent() && store.Block().Dominates(at.Block())
}

// readVariable returns the roots of the variable ptr, or of its field path,
// as a read at at sees them.
func readVariable(ctx *Context, ptr ssa.Value, path []int, at ssa.Instruction, loopInfo *cfg.LoopInfo) []ssa.Value {
	if len(path) == 0 {
		return ctx.RootTracer.PointeeRoots(ptr, at, loopInfo)
	}
	var roots []ssa.Value
	if alloc, ok := ptr.(*ssa.Alloc); ok {
		for _, v := range ctx.RootTracer.ParamReadSelected(alloc, path, at) {
			roots = append(roots, ctx.RootTracer.MutableRoots(v, loopInfo)...)
		}
	}
	return roots
}

// readPoint returns where the variables that the literal mc captures are
// read when it is handed on at at: at itself, or, when at is in a closure
// nested in the function that makes mc, where that closure runs there. That
// is its one plain call when it has one and runs nowhere else
// (callsite.ClosureCalls), or where it is made otherwise. A literal made
// where at is not nested is read where it is made.
func (c *captureCache) readPoint(at ssa.Instruction, mc *ssa.MakeClosure) ssa.Instruction {
	for at.Parent() != mc.Parent() {
		next := callsite.FreeVarMakeClosure(at.Parent())
		if next == nil {
			return mc
		}
		at = next
		calls, done := c.calls[next]
		if !done {
			calls = callsite.ClosureCalls(next)
			c.calls[next] = calls
		}
		if len(calls) == 1 {
			at = calls[0]
		}
	}
	return at
}

// summaryBuilder finds what a literal returns itself. It follows merges, a
// nested literal called in place, and a captured variable or a field of one,
// and it records the literals called through a variable (calls), whose
// summaries summarySCC adds. go/ssa
// captures every variable by reference, so a captured value is a load of a
// FreeVar, or of a field of one. A variable counts when some path reaches the
// load without assigning it (unassignedAt): a literal that assigns it first
// returns its own chain, which is a use in its body. A variable that a
// closure, other than one called in place, may assign is left out, since when
// that closure runs is not known. So is a struct variable the literal writes
// to, and a variable whose value the literal's body branches.
type summaryBuilder struct {
	c *captureCache
	// inside holds the literals followed. A root defined in one of them is
	// the literal's own chain or parameter.
	inside   []*ssa.Function
	followed map[*ssa.MakeClosure]bool
	seen     map[ssa.Value]bool
	sum      literalSummary
	// calls holds the literals the literal calls through a variable, whose
	// summaries summarySCC adds.
	calls []*ssa.MakeClosure
}

// literalFrame is a function literal whose returns summaryBuilder follows.
// outer is the frame of the literal that calls it in place at call. It is nil
// for a literal whose captured variables are read where the literal is
// handed on.
type literalFrame struct {
	fn    *ssa.Function
	mc    *ssa.MakeClosure
	outer *literalFrame
	call  ssa.Instruction
}

// follow visits the returns of the literal that mc makes, once.
func (b *summaryBuilder) follow(mc *ssa.MakeClosure, outer *literalFrame, call ssa.Instruction) {
	fn := mc.Fn.(*ssa.Function)
	if !typeutil.HasGormDBResult(fn.Signature.Results()) || b.followed[mc] {
		return
	}
	b.followed[mc] = true
	b.inside = append(b.inside, fn)
	f := &literalFrame{fn: fn, mc: mc, outer: outer, call: call}
	for _, block := range fn.Blocks {
		ret, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return)
		if !ok {
			continue
		}
		for _, r := range ret.Results {
			if typeutil.IsGormDB(r.Type()) {
				b.visit(r, f)
			}
		}
	}
}

// visit adds what r, a value of f.fn, holds as captured.
func (b *summaryBuilder) visit(r ssa.Value, f *literalFrame) {
	if b.seen[r] {
		return
	}
	b.seen[r] = true
	switch r := r.(type) {
	case *ssa.Phi:
		for _, e := range r.Edges {
			b.visit(e, f)
		}
	case *ssa.Call:
		// A nested literal called in place returns what it returns. One
		// called through a variable, as a getter chain is, reads its captured
		// variables where the literal is handed on.
		if mc, ok := r.Call.Value.(*ssa.MakeClosure); ok && mc.Parent() == f.fn {
			b.follow(mc, f, r)
			return
		}
		for _, mc := range handedOnLiterals(r.Call.Value, b.c.ctx, map[ssa.Value]bool{}) {
			if !slices.Contains(b.calls, mc) {
				b.calls = append(b.calls, mc)
			}
		}
	case *ssa.UnOp:
		if fv, path, ok := capturedPath(r.X); ok {
			b.visitCaptured(fv, path, f, r)
			return
		}
		b.addOutside(b.c.ctx.RootTracer.MutableRoots(r, nil))
	default:
		b.addOutside(b.c.ctx.RootTracer.MutableRoots(r, nil))
	}
}

// capturedPath returns the captured variable that addr is, or a field of,
// and the field path from it: a FreeVar, or a FieldAddr chain on one.
func capturedPath(addr ssa.Value) (*ssa.FreeVar, []int, bool) {
	var path []int
	for {
		switch a := addr.(type) {
		case *ssa.FreeVar:
			return a, path, true
		case *ssa.FieldAddr:
			path = append([]int{a.Field}, path...)
			addr = a.X
		default:
			return nil, nil, false
		}
	}
}

// visitCaptured adds a variable that f captures by reference, or the field
// path of it, loaded at load. It follows the variable out through the frames
// that call f in place, to where the literal's own function captures it.
func (b *summaryBuilder) visitCaptured(fv *ssa.FreeVar, path []int, f *literalFrame, load ssa.Instruction) {
	c, at := b.c, load
	for {
		if len(path) > 0 && c.writesCaptured(fv, true) || !c.unassignedAt(fv, at) || c.assignedByOtherClosure(fv) || c.branchesCaptured(fv) {
			return
		}
		ptr := f.mc.Bindings[slices.Index(f.fn.FreeVars, fv)]
		if f.outer == nil {
			b.addRead(capturedRead{ptr: ptr, path: path, key: fmt.Sprint(path), mc: f.mc})
			return
		}
		next, ok := ptr.(*ssa.FreeVar)
		if !ok {
			// A variable of the outer literal's own body, read where the
			// nested literal is made.
			b.addOutside(readVariable(c.ctx, ptr, path, f.mc, nil))
			return
		}
		fv, at, f = next, f.call, f.outer
	}
}

// addRead adds read once.
func (b *summaryBuilder) addRead(read capturedRead) {
	if !slices.ContainsFunc(b.sum.reads, func(r capturedRead) bool {
		return r.ptr == read.ptr && r.key == read.key && r.mc == read.mc
	}) {
		b.sum.reads = append(b.sum.reads, read)
	}
}

// addOutside adds the roots that are not defined in a followed literal or in
// a function nested in one. A root inside one is the literal's own chain or
// parameter, which its body uses.
func (b *summaryBuilder) addOutside(roots []ssa.Value) {
	for _, root := range roots {
		block := tracer.RootBlock(root)
		if (block == nil || !slices.ContainsFunc(b.inside, func(fn *ssa.Function) bool { return nestedIn(block.Parent(), fn) })) && !slices.Contains(b.sum.roots, root) {
			b.sum.roots = append(b.sum.roots, root)
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

// assignFlow is where the captured variable of a function may be unassigned
// (see unassignedAt): the blocks it may enter unassigned, and the index of the
// first instruction of each block that assigns it. never is set for a
// variable that nothing in the function may assign, which is unassigned
// everywhere.
type assignFlow struct {
	never bool
	entry map[*ssa.BasicBlock]bool
	first map[*ssa.BasicBlock]int
}

// unassignedAt reports whether some path from the entry of at's function
// reaches at without assigning the captured variable fv: without a store to
// it, or a call in place of a literal that assigns it on every path.
func (c *captureCache) unassignedAt(fv *ssa.FreeVar, at ssa.Instruction) bool {
	flow := c.flow(fv)
	block := at.Block()
	if flow.never {
		return true
	}
	if !flow.entry[block] {
		return false
	}
	first, assigned := flow.first[block]
	return !assigned || c.indexOf(at) <= first
}

// flow returns the assignFlow of fv. It is found forward from the blocks
// with no predecessor, the entry and the recover block.
func (c *captureCache) flow(fv *ssa.FreeVar) *assignFlow {
	if flow, done := c.flows[fv]; done {
		return flow
	}
	// Only a store to it, or a literal that captures it, may assign it.
	flow := &assignFlow{never: !slices.ContainsFunc(*fv.Referrers(), func(r ssa.Instruction) bool {
		switch r.(type) {
		case *ssa.Store, *ssa.MakeClosure:
			return true
		}
		return false
	})}
	if flow.never {
		c.flows[fv] = flow
		return flow
	}
	flow.entry, flow.first = map[*ssa.BasicBlock]bool{}, map[*ssa.BasicBlock]int{}
	var work []*ssa.BasicBlock
	for _, block := range fv.Parent().Blocks {
		if i := slices.IndexFunc(block.Instrs, func(instr ssa.Instruction) bool { return c.assignsCaptured(instr, fv) }); i >= 0 {
			flow.first[block] = i
		}
		if len(block.Preds) == 0 {
			flow.entry[block] = true
			work = append(work, block)
		}
	}
	for len(work) > 0 {
		block := work[len(work)-1]
		work = work[:len(work)-1]
		if _, assigned := flow.first[block]; assigned {
			continue
		}
		for _, succ := range block.Succs {
			if !flow.entry[succ] {
				flow.entry[succ] = true
				work = append(work, succ)
			}
		}
	}
	c.flows[fv] = flow
	return flow
}

// indexOf returns the index of instr in its block.
func (c *captureCache) indexOf(instr ssa.Instruction) int {
	if _, done := c.index[instr]; !done {
		for i, in := range instr.Block().Instrs {
			c.index[in] = i
		}
	}
	return c.index[instr]
}

// assignsCaptured reports whether instr assigns the captured variable fv: a
// store to it, or a call in place of a literal that captures it and assigns
// it on every path to a return. The literal is nested in fv's function, so
// the question goes down a finite nesting.
func (c *captureCache) assignsCaptured(instr ssa.Instruction, fv *ssa.FreeVar) bool {
	switch i := instr.(type) {
	case *ssa.Store:
		return i.Addr == fv
	case *ssa.Call:
		mc, ok := i.Call.Value.(*ssa.MakeClosure)
		if !ok || !slices.Contains(mc.Bindings, ssa.Value(fv)) {
			return false
		}
		inner := capturedAs(mc, fv)
		return !slices.ContainsFunc(inner.Parent().Blocks, func(block *ssa.BasicBlock) bool {
			ret, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return)
			return ok && c.unassignedAt(inner, ret)
		})
	}
	return false
}

// assignedByOtherClosure reports whether a closure that fv's function makes
// may assign the captured variable fv, other than one it only calls in place
// (callsite.ClosureOnlyCalled), whose assignments unassignedAt reads. Such a
// closure may run at any time.
func (c *captureCache) assignedByOtherClosure(fv *ssa.FreeVar) bool {
	if assigned, done := c.others[fv]; done {
		return assigned
	}
	assigned := slices.ContainsFunc(*fv.Referrers(), func(ref ssa.Instruction) bool {
		mc, ok := ref.(*ssa.MakeClosure)
		if !ok {
			return false
		}
		inner := capturedAs(mc, fv)
		if callsite.ClosureOnlyCalled(mc) {
			return c.assignedByOtherClosure(inner)
		}
		return c.writesCaptured(inner, false)
	})
	c.others[fv] = assigned
	return assigned
}

// writesCaptured reports whether fv's function, or a closure it makes,
// stores to the captured variable fv, or, with fields, into a field of it.
func (c *captureCache) writesCaptured(fv *ssa.FreeVar, fields bool) bool {
	key := capturedWriteKey{fv, fields}
	if writes, done := c.writes[key]; done {
		return writes
	}
	var writesTo func(addr ssa.Value) bool
	writesTo = func(addr ssa.Value) bool {
		return slices.ContainsFunc(*addr.Referrers(), func(r ssa.Instruction) bool {
			switch r := r.(type) {
			case *ssa.Store:
				return r.Addr == addr
			case *ssa.FieldAddr:
				return fields && writesTo(r)
			case *ssa.MakeClosure:
				return c.writesCaptured(capturedAs(r, addr), fields)
			}
			return false
		})
	}
	writes := writesTo(fv)
	c.writes[key] = writes
	return writes
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
func (c *captureCache) branchesCaptured(fv *ssa.FreeVar) bool {
	if branches, done := c.branches[fv]; done {
		return branches
	}
	branches := slices.ContainsFunc(*fv.Referrers(), func(ref ssa.Instruction) bool {
		switch r := ref.(type) {
		case *ssa.UnOp:
			return slices.ContainsFunc(*r.Referrers(), func(use ssa.Instruction) bool {
				call, ok := use.(ssa.CallInstruction)
				if !ok {
					return false
				}
				site := callsite.SiteOfWithoutFuncVars(call)
				return site.IsGormMethod() && site.Recv == ssa.Value(r) && !typeutil.IsImmutableReturningBuiltin(site.Method()) && !typeutil.IsStateOnlyMethod(site.Method())
			})
		case *ssa.MakeClosure:
			return c.branchesCaptured(capturedAs(r, fv))
		}
		return false
	})
	c.branches[fv] = branches
	return branches
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
