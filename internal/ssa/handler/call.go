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
	"slices"

	"golang.org/x/tools/go/ssa"

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
func isAssignment(call *ssa.Call, root ssa.Value, ctx *Context) bool {
	if isLoopFieldRebuild(call, ctx) {
		return true
	}
	visited := map[ssa.Value]bool{call: true}
	work := []ssa.Value{call}
	for len(work) > 0 {
		v := work[len(work)-1]
		work = work[:len(work)-1]

		for _, user := range *v.Referrers() {
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
			if merge != nil && ctx.RootTracer.HasMutableRoot(merge, ctx.LoopInfo, root) {
				return true
			}
			if next != nil && !visited[next] {
				visited[next] = true
				work = append(work, next)
			}
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

// roots returns the mutable roots that v may hold: first the one it resolves
// to (FindMutableRoot), then the others that FindAllMutableRoots finds through
// a merge. It is nil when v has no mutable root.
//
// It is also nil when FindMutableRoot finds no root but FindAllMutableRoots
// does. That list holds what a merge may hold, immutable values included, and
// the roots inside a closure that an immutable-return directive vouches for,
// which FindMutableRoot trusts. So FindMutableRoot decides whether v is
// mutable; it tries every value a merge or a variable in memory may hold
// (#340).
func (u siteUse) roots(v ssa.Value) []ssa.Value {
	root := u.ctx.RootTracer.FindMutableRoot(v, u.ctx.LoopInfo)
	if root == nil {
		return nil
	}
	roots := []ssa.Value{root}
	for _, r := range u.ctx.RootTracer.FindAllMutableRoots(v, u.ctx.LoopInfo) {
		if r != root {
			roots = append(roots, r)
		}
	}
	return roots
}

// checkRoots reports a violation at the site for each of roots that is already
// changed where the site runs. A plain call leaves its first root to
// DetectViolations, which orders the uses by position. Only the first root is
// checked against the branches made through a merge that may hold it
// (RecordMayBranch). It must run before the site's own uses are recorded:
// they would count as the earlier change.
func (u siteUse) checkRoots(roots []ssa.Value) {
	t := u.ctx.Tracker
	for i, r := range roots {
		if i == 0 && u.ordered() {
			continue
		}
		var polluted bool
		if u.site.Mode == callsite.SiteDefer {
			polluted = t.IsPollutedAnywhere(r) || i == 0 && t.IsMayBranchedAnywhere(r)
		} else {
			polluted = t.IsPollutedAt(r, u.block) || i == 0 && t.IsMayBranchedAt(r, u.block)
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
	u.checkRoots(roots)
	if len(roots) > 0 && u.ordered() {
		u.ctx.Tracker.RecordPureUse(roots[0], use)
	}
}

// branch records the site as a branch of roots, unless assigns reports that
// the call's result reassigns the variable that holds that root (see
// isAssignment). The first root gets the use itself, and the other roots get
// a branch through a merge that may hold them (#252). A branch made on every
// iteration of a loop that a root is defined outside of is reported at once,
// for each of the roots.
func (u siteUse) branch(roots []ssa.Value, use pollution.UsageInfo, assigns func(root ssa.Value) bool) {
	t := u.ctx.Tracker
	u.checkRoots(roots)
	for i, r := range roots {
		switch {
		case assigns(r):
			// Assignment creates new root - record but doesn't pollute
			if i == 0 {
				t.RecordAssignment(r, use)
			}
		case i > 0:
			if u.definedBefore(r) {
				t.RecordMayBranch(r, use, u.ordered())
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
		u.branch(roots, pollution.UsageInfo{Block: u.block, Pos: u.pos, Escapes: true}, assigns)
	}
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
// *gorm.DB to a //gormreuse:immutable-param parameter of the function named
// name. When a Session would drop state the argument carries (dropsState,
// see tracer.RootTracer.SessionDropsState), it does not advise one (#344).
func immutableParamContractMessage(name string, dropsState bool) string {
	msg := "mutable *gorm.DB passed to //gormreuse:immutable-param parameter of " + name
	if dropsState {
		return msg + "; a Session would drop its Attrs, Assign or InstanceSet, so pass a value built for this call instead"
	}
	return msg + "; isolate it with .Session(&gorm.Session{}) before passing"
}

// sendHandler handles *ssa.Send instructions.
type sendHandler struct{}

// handle marks *gorm.DB sent to channels as polluted.
// Handles both direct sends and sends through MakeInterface (chan interface{}).
func (h *sendHandler) handle(send *ssa.Send, ctx *Context) {
	gormVal, kind := pollutionsource.Leak(send, ctx.RootTracer.FuncVars())
	if kind == pollutionsource.KindNone {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(gormVal, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, send.Block(), ctx.pos(send.Pos()))
}

// storeHandler handles *ssa.Store instructions.
type storeHandler struct{}

// handle marks *gorm.DB stored to slice elements as polluted.
// Handles both direct stores and stores through MakeInterface ([]interface{}).
//
// The read-only variadic stdlib exemption (fmt.Println(q), log.Printf, t.Logf)
// lives in pollutionsource.Leak so the purity validator honors it too.
func (h *storeHandler) handle(store *ssa.Store, ctx *Context) {
	gormVal, kind := pollutionsource.Leak(store, ctx.RootTracer.FuncVars())
	if kind == pollutionsource.KindNone {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(gormVal, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, store.Block(), ctx.pos(store.Pos()))
}

// mapUpdateHandler handles *ssa.MapUpdate instructions.
type mapUpdateHandler struct{}

// handle marks *gorm.DB stored in maps as polluted.
// Handles both direct stores and stores through MakeInterface (map[K]interface{}).
func (h *mapUpdateHandler) handle(mapUpdate *ssa.MapUpdate, ctx *Context) {
	gormVal, kind := pollutionsource.Leak(mapUpdate, ctx.RootTracer.FuncVars())
	if kind == pollutionsource.KindNone {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(gormVal, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mapUpdate.Block(), ctx.pos(mapUpdate.Pos()))
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
