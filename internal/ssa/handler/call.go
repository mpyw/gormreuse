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
//	│  *ssa.Call         │  callHandler      │  Method calls, function calls  │
//	│  *ssa.Go           │  goHandler        │  go func() { ... }             │
//	│  *ssa.Defer        │  deferHandler     │  defer func() { ... }          │
//	│  *ssa.Send         │  sendHandler      │  ch <- db (channel send)       │
//	│  *ssa.Store        │  storeHandler     │  slice[i] = db (slice elem)    │
//	│  *ssa.MapUpdate    │  mapUpdateHandler │  map[k] = db (map storage)     │
//	│  *ssa.MakeInterface│  makeInterfaceHandler │ interface{}(db)            │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// # Type Switch Dispatch
//
// The Dispatch function uses type switch for O(1) dispatch to handlers:
//
//	switch i := instr.(type) {
//	case *ssa.Call:    (&callHandler{}).handle(i, ctx)
//	case *ssa.Go:      (&goHandler{}).handle(i, ctx)
//	...
//	}
//
// This is more efficient than reflection-based dispatch for a small number
// of known types.
package handler

import (
	"go/token"
	"slices"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
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
	CFG        *cfg.Analyzer      // Control flow graph analysis
	LoopInfo   *cfg.LoopInfo      // Loop detection results for current function
	CurrentFn  *ssa.Function      // The function being analyzed

	// NeedsImmutableParam holds the functions marked //gormreuse:immutable-param
	// that genuinely rely on immutability (they branch a *gorm.DB parameter). A
	// call passing a mutable *gorm.DB to such a function violates the contract
	// (Phase 1b stage 2b). Functions whose annotation is redundant (never branch
	// a param) are excluded, so they impose no caller-side obligation.
	NeedsImmutableParam map[*ssa.Function]bool

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

// callHandler handles *ssa.Call instructions.
//
// This is the most complex handler, covering:
//   - Direct gorm method calls: q.Find(nil)
//   - Bound method calls: find := q.Find; find(nil)
//   - Function calls with *gorm.DB arguments
//
// # Uniform Processing
//
// Every gorm chain method use is processed uniformly:
//   - First use from a root: OK, marks root polluted
//   - Second+ use from same root: VIOLATION at that call site
//
// There's no distinction between "terminal" and "non-terminal" methods.
// All uses are recorded, and violations are detected via CFG reachability.
type callHandler struct{}

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
			if merge != nil && slices.Contains(ctx.RootTracer.FindAllMutableRoots(merge, ctx.LoopInfo), root) {
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
	if ctx.LoopInfo == nil || !ctx.LoopInfo.IsInLoop(call.Block()) || len(call.Call.Args) == 0 {
		return false
	}
	load, ok := call.Call.Args[0].(*ssa.UnOp)
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
// v as receiver (i.e., they form a method chain).
func isChainedGormMethodCall(v ssa.Value, nextCall *ssa.Call) bool {
	// Check if nextCall is a gorm method call
	callee := nextCall.Call.StaticCallee()
	if callee == nil {
		return false
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil {
		return false
	}

	if !typeutil.IsGormDB(sig.Recv().Type()) {
		return false
	}

	// Check if v is nextCall's receiver (first argument)
	if len(nextCall.Call.Args) == 0 {
		return false
	}

	return nextCall.Call.Args[0] == v
}

// handle processes a Call instruction and tracks *gorm.DB pollution.
//
// Processing order:
//  1. Check function calls with *gorm.DB args (mark as polluted)
//  2. Handle bound method calls (find := q.Find; find(nil))
//  3. Process gorm method calls: pure/assignment/actual use
//  4. Check all Phi roots for conditional merges
func (h *callHandler) handle(call *ssa.Call, ctx *Context) {
	isInLoop := ctx.LoopInfo.IsInLoop(call.Block())

	// Check function call pollution (non-gorm-method calls with *gorm.DB args)
	h.checkFunctionCallPollution(call, ctx)

	// Check bound method calls (method values)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		h.processBoundMethodCall(call, mc, isInLoop, ctx)
		return
	}

	// Check gorm method calls
	if !h.isGormDBMethodCall(call) {
		return
	}

	callee := call.Call.StaticCallee()
	if callee == nil {
		return
	}

	methodName := callee.Name()
	isImmutableReturning := typeutil.IsImmutableReturningBuiltin(methodName)

	// Get receiver
	if len(call.Call.Args) == 0 {
		return
	}
	recv := call.Call.Args[0]

	// pos is the effective position (call-site override inside an invoked
	// closure, else the call's own position) — see Context.pos / #68.
	pos := ctx.pos(call.Pos())

	// A receiver that is a Session/Debug copy reads the Statement of the root
	// it was copied from (#174). This holds even though the copy itself is
	// immutable, so it is checked before the immutable-source return below.
	recordCallAliasUses(recv, call.Block(), pos, ctx)

	// Find mutable root
	root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
	if root == nil {
		return // Immutable source
	}

	// KEY CHANGE: Process ALL calls uniformly (no isTerminal skip)
	// Record usage (violations detected later in DetectViolations).
	use := pollution.UsageInfo{Block: call.Block(), Pos: pos, Method: methodName, Call: call.Pos()}
	if isImmutableReturning {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, use)
	} else if isAssignment(call, root, ctx) {
		// Assignment creates new root - record but doesn't pollute
		ctx.Tracker.RecordAssignment(root, use)
	} else {
		// Actual use - pollutes the root
		ctx.Tracker.ProcessBranch(root, use)

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
	}

	// Check ALL possible roots for phi nodes
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv, ctx.LoopInfo)
	for _, r := range allRoots {
		if r == call && isLoopFieldRebuild(call, ctx) {
			// The call reads its own result from the previous iteration
			// through the field (#182). A use of that result later in the
			// loop body runs before this call on the next iteration.
			ctx.Tracker.RecordLoopCarriedUse(call, call.Block(), pos)
		}
		if r == root {
			continue
		}
		if ctx.Tracker.IsPollutedAt(r, call.Block()) {
			ctx.Tracker.AddViolationWithRoot(pos, r)
		}
	}
}

// processBoundMethodCall handles method values like: find := q.Find; find(nil)
//
// In SSA, method values are MakeClosure with receiver in Bindings[0] and
// method name suffixed with "$bound". We extract the receiver and track
// pollution like normal method calls.
//
// Example:
//
//	q := db.Where("x")
//	find := q.Find  // MakeClosure(Find$bound, [q])
//	find(nil)       // first use - OK
//	q.Count(nil)    // VIOLATION (q already polluted by find(nil))
func (h *callHandler) processBoundMethodCall(call *ssa.Call, mc *ssa.MakeClosure, isInLoop bool, ctx *Context) {
	if len(mc.Bindings) == 0 {
		return
	}

	recv := mc.Bindings[0]
	if !typeutil.IsGormDB(recv.Type()) {
		return
	}

	methodName := strings.TrimSuffix(mc.Fn.Name(), "$bound")
	isImmutableReturning := typeutil.IsImmutableReturningBuiltin(methodName)

	pos := ctx.pos(call.Pos())
	recordCallAliasUses(recv, call.Block(), pos, ctx)

	root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
	if root == nil {
		return
	}

	// Get ALL possible roots BEFORE recording usage (needed for pollution check)
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv, ctx.LoopInfo)

	// Check if ANY root was already polluted BEFORE this call
	for _, r := range allRoots {
		if ctx.Tracker.IsPollutedAt(r, call.Block()) {
			ctx.Tracker.AddViolationWithRoot(pos, r)
		}
	}

	// Record usage (violations detected later)
	use := pollution.UsageInfo{Block: call.Block(), Pos: pos, Method: methodName, Call: call.Pos()}
	if isImmutableReturning {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, use)
	} else {
		// Non-pure methods pollute the root
		ctx.Tracker.ProcessBranch(root, use)

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
	}
}

// checkFunctionCallPollution marks *gorm.DB args passed to non-gorm functions as polluted.
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
func (h *callHandler) checkFunctionCallPollution(call *ssa.Call, ctx *Context) {
	callee := ctx.RootTracer.Callee(&call.Call)

	// A Session/Debug copy passed to a function may be read there, pure or not.
	// The receiver of a gorm method is handled in handle.
	if !h.isGormDBMethodCall(call) {
		for _, arg := range call.Call.Args {
			if gormArg, ok := pollutionsource.UnwrapGormDB(arg); ok {
				recordCallAliasUses(gormArg, call.Block(), ctx.pos(call.Pos()), ctx)
			}
		}
	}

	// A *gorm.DB passed to a read-only parameter of a gorm method, such as a
	// subquery, is read, not branched (#185).
	readOnlyArgs, readStart, hasReadOnly := pollutionsource.CallReadOnlyArgs(&call.Call)
	for _, v := range readOnlyArgs {
		recordArgumentRead(v, call, ctx)
	}

	// Check if this is a pure function - pure functions don't pollute args
	if callee != nil && ctx.RootTracer.IsPureFunction(callee) {
		return
	}

	// Note: We don't skip gorm methods here because we need to pollute
	// *gorm.DB arguments passed through interface{} (e.g., base.Model(q)),
	// except the read-only ones recorded above.
	// The receiver of a gorm method, Args[0], is recorded by handle. Recording
	// it here too gave every use a second copy, and the fix generator then
	// counted a reassigning call as a use of the root it creates (#202).
	firstArg := 0
	if h.isGormDBMethodCall(call) {
		firstArg = 1
	}

	// If the function returns *gorm.DB and result is assigned, treat like gorm method assignment.
	// The assignment creates a new mutable root, so we shouldn't ADD pollution to args.
	// However, we still need to CHECK if args are already polluted (to detect reuse).
	// This enables patterns like: q = buildQuery(q, "filter")
	// A tuple result such as (*gorm.DB, error) counts too: q, err = helper(q).
	returnsGormDB := typeutil.HasGormDBResult(call.Type())

	for i, arg := range call.Call.Args {
		if i < firstArg {
			continue
		}
		if hasReadOnly && i >= readStart {
			continue // read above
		}
		// Check if arg is *gorm.DB (directly or wrapped in MakeInterface)
		gormArg, ok := pollutionsource.UnwrapGormDB(arg)
		if !ok {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(gormArg, ctx.LoopInfo)
		if root == nil {
			continue
		}

		// Caller-side contract check (Phase 1b stage 2b): passing a mutable
		// (root != nil) *gorm.DB to a parameter of a function that relies on
		// immutability — it branches the parameter — is unsafe: the callee's
		// internal branching interferes because the value is not isolated.
		if checksImmutableParamArg(callee, i, ctx) {
			ctx.Tracker.AddMessageViolation(ctx.pos(call.Pos()), immutableParamContractMessage(callee))
		}

		if returnsGormDB && isAssignment(call, root, ctx) {
			// For reassignment pattern: check if already polluted, but don't add pollution.
			// This is similar to how gorm methods handle RecordAssignment.
			ctx.Tracker.RecordAssignment(root, pollution.UsageInfo{Block: call.Block(), Pos: ctx.pos(call.Pos())})
		} else {
			// Mark polluted (function may use the value)
			ctx.Tracker.MarkPolluted(root, call.Block(), ctx.pos(call.Pos()))
		}
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

// recordArgumentRead records v, passed to a read-only parameter of the gorm
// method call, as a pure use of its root (#185): a branch of the root before
// the call shows up in the read. A change after the call is found from the
// uses of the call's result, through FindAliasedRoots.
func recordArgumentRead(v ssa.Value, call *ssa.Call, ctx *Context) {
	block, pos := call.Block(), ctx.pos(call.Pos())
	recordCallAliasUses(v, block, pos, ctx)

	root := ctx.RootTracer.FindMutableRoot(v, ctx.LoopInfo)
	if root == nil {
		return
	}
	ctx.Tracker.RecordPureUse(root, pollution.UsageInfo{Block: block, Pos: pos})
	for _, r := range ctx.RootTracer.FindAllMutableRoots(v, ctx.LoopInfo) {
		if r != root && ctx.Tracker.IsPollutedAt(r, block) {
			ctx.Tracker.AddViolationWithRoot(pos, r)
		}
	}
}

// checksImmutableParamArg reports whether argument i of a call to callee is
// bound to a parameter under the caller-side //gormreuse:immutable-param
// contract: callee (or the function it stands in for, see
// directive.DeclaredFunc) branches its parameters. A method call carries its
// receiver as Args[0]; the directive governs parameters, not the receiver, so
// that argument is skipped. A bound method wrapper has no receiver argument.
func checksImmutableParamArg(callee *ssa.Function, i int, ctx *Context) bool {
	if callee == nil || !ctx.NeedsImmutableParam[directive.DeclaredFunc(callee)] {
		return false
	}
	return callee.Signature.Recv() == nil || i != 0
}

// immutableParamContractMessage builds the diagnostic for passing a mutable
// *gorm.DB to a //gormreuse:immutable-param parameter.
func immutableParamContractMessage(callee *ssa.Function) string {
	name := "immutable-param function"
	if callee != nil {
		name = directive.DeclaredFunc(callee).Name()
	}
	return "mutable *gorm.DB passed to //gormreuse:immutable-param parameter of " + name +
		"; isolate it with .Session(&gorm.Session{}) before passing"
}

func (h *callHandler) isGormDBMethodCall(call *ssa.Call) bool {
	callee := call.Call.StaticCallee()
	if callee == nil {
		return false
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil {
		return false
	}

	return typeutil.IsGormDB(sig.Recv().Type())
}

// goHandler handles *ssa.Go instructions.
type goHandler struct{}

// handle processes a Go instruction.
func (h *goHandler) handle(g *ssa.Go, ctx *Context) {
	block := g.Block()
	processGormDBCallCommonWith(&g.Call, g.Pos(), block, ctx, func(root ssa.Value) bool {
		return ctx.Tracker.IsPollutedAt(root, block)
	})
}

// deferHandler handles *ssa.Defer instructions.
type deferHandler struct{}

// handle processes a Defer instruction.
// Defer uses IsPollutedAnywhere because it executes at function exit.
func (h *deferHandler) handle(d *ssa.Defer, ctx *Context) {
	processGormDBCallCommonWith(&d.Call, d.Pos(), d.Block(), ctx, func(root ssa.Value) bool {
		return ctx.Tracker.IsPollutedAnywhere(root)
	})
}

// sendHandler handles *ssa.Send instructions.
type sendHandler struct{}

// handle marks *gorm.DB sent to channels as polluted.
// Handles both direct sends and sends through MakeInterface (chan interface{}).
func (h *sendHandler) handle(send *ssa.Send, ctx *Context) {
	gormVal, kind := pollutionsource.Leak(send)
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
	gormVal, kind := pollutionsource.Leak(store)
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
	gormVal, kind := pollutionsource.Leak(mapUpdate)
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

// pollutionChecker is a function that checks if a root is polluted.
type pollutionChecker func(root ssa.Value) bool

// Dispatch routes SSA instructions to their handlers using type switch.
//
// Handler mapping:
//   - *ssa.Call         → callHandler (method/function calls)
//   - *ssa.Go           → goHandler (goroutine launches)
//   - *ssa.Send         → sendHandler (channel send: ch <- db)
//   - *ssa.Store        → storeHandler (slice store: slice[i] = db)
//   - *ssa.MapUpdate    → mapUpdateHandler (map store: m[k] = db)
//   - *ssa.MakeInterface → makeInterfaceHandler (interface conversion)
//
// Note: *ssa.Defer uses DispatchDefer (different pollution semantics).
func Dispatch(instr ssa.Instruction, ctx *Context) {
	switch i := instr.(type) {
	case *ssa.Call:
		(&callHandler{}).handle(i, ctx)
	case *ssa.Go:
		(&goHandler{}).handle(i, ctx)
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
// Defer executes at function exit (not where declared), so it uses
// IsPollutedAnywhere instead of IsPollutedAt to check pollution across
// the entire function.
//
// Example:
//
//	q := db.Where("x")
//	defer q.Find(nil)  // deferred - executes at function exit
//	q.Count(nil)       // executes BEFORE defer, pollutes q
//	// function exits → defer q.Find(nil) → VIOLATION!
func DispatchDefer(d *ssa.Defer, ctx *Context) {
	(&deferHandler{}).handle(d, ctx)
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
	(&goHandler{}).handle(g, ctx)
}

// processGormDBCallCommonWith processes gorm calls with a custom pollution checker.
//
// Used by the defer and goroutine handlers. In addition to CHECKING whether the
// receiver/argument root is already polluted (and reporting a violation if so),
// it RECORDS each use as a branch use. Recording lets a later defer/goroutine
// observe an earlier one, so patterns whose ONLY uses are deferred/spawned —
// e.g. `defer q.Find(nil); defer q.Count(nil)` — are detected. Branch uses are
// excluded from position-ordered detection (see pollution.Tracker.branchUses).
func processGormDBCallCommonWith(callCommon *ssa.CallCommon, pos token.Pos, block *ssa.BasicBlock, ctx *Context, isPolluted pollutionChecker) {
	callee := ctx.RootTracer.Callee(callCommon)
	if callee == nil {
		return
	}

	sig := callee.Signature

	// Method call on *gorm.DB
	if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		if len(callCommon.Args) == 0 {
			return
		}
		handleBranchUse(callCommon.Args[0], pos, block, ctx, isPolluted)
		return
	}

	// Function call with *gorm.DB arguments
	for i, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}
		// The caller-side immutable-param contract holds for a deferred or
		// spawned call as for a plain one (#192).
		if checksImmutableParamArg(callee, i, ctx) && ctx.RootTracer.FindMutableRoot(arg, ctx.LoopInfo) != nil {
			ctx.Tracker.AddMessageViolation(ctx.pos(pos), immutableParamContractMessage(callee))
		}
		handleBranchUse(arg, pos, block, ctx, isPolluted)
	}
}

// handleBranchUse handles one *gorm.DB value (a receiver or an argument) of a
// deferred or spawned call. It reports a violation for each of the value's
// possible roots that is already polluted, then records the use as a branch use.
func handleBranchUse(v ssa.Value, pos token.Pos, block *ssa.BasicBlock, ctx *Context, isPolluted pollutionChecker) {
	root := ctx.RootTracer.FindMutableRoot(v, ctx.LoopInfo)
	if root == nil {
		return
	}

	if isPolluted(root) {
		ctx.Tracker.AddViolationWithRoot(pos, root)
	}

	// Check ALL possible roots for phi nodes
	allRoots := ctx.RootTracer.FindAllMutableRoots(v, ctx.LoopInfo)
	for _, r := range allRoots {
		if r == root {
			continue
		}
		if isPolluted(r) {
			ctx.Tracker.AddViolationWithRoot(pos, r)
		}
	}

	// Record this deferred/spawned use so a later defer/go sees it.
	ctx.Tracker.RecordBranchUse(root, block, pos)
}
