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
//	│  *ssa.Call         │  CallHandler      │  Method calls, function calls  │
//	│  *ssa.Go           │  GoHandler        │  go func() { ... }             │
//	│  *ssa.Defer        │  DeferHandler     │  defer func() { ... }          │
//	│  *ssa.Send         │  SendHandler      │  ch <- db (channel send)       │
//	│  *ssa.Store        │  StoreHandler     │  slice[i] = db (slice elem)    │
//	│  *ssa.MapUpdate    │  MapUpdateHandler │  map[k] = db (map storage)     │
//	│  *ssa.MakeInterface│  MakeInterfaceHandler │ interface{}(db)            │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// # Type Switch Dispatch
//
// The Dispatch function uses type switch for O(1) dispatch to handlers:
//
//	switch i := instr.(type) {
//	case *ssa.Call:    (&CallHandler{}).Handle(i, ctx)
//	case *ssa.Go:      (&GoHandler{}).Handle(i, ctx)
//	...
//	}
//
// This is more efficient than reflection-based dispatch for a small number
// of known types.
package handler

import (
	"go/token"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
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
}

// CallHandler handles *ssa.Call instructions.
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
type CallHandler struct{}

// isAssignment checks if a call result is assigned to create a new root.
// Returns true if the call is used in an assignment (Store to Alloc or Phi).
//
// KEY INSIGHT: All Phi nodes (loop-header, loop-internal, and non-loop) represent assignments:
//   - They merge control flow and create new SSA values
//   - Assignment does NOT pollute the original root
//   - Only actual method calls (non-assignment) pollute
//
// Example patterns:
//   q = q.Where("x")  // Assignment - no pollution
//   q.Find(nil)       // Actual use - pollutes q
//
// Loop patterns:
//   for { q = q.Where() }         // Loop-header Phi - assignment, no pollution
//   for { if x { q = q.Where() }} // Loop-internal Phi - assignment, no pollution
//   for { q.Find(nil) }           // Actual use in loop - pollutes q
//
// This works recursively for arbitrary nesting depths (for-if-if-if-for-for-if-if etc.)
// because SSA naturally represents all control flow as Phi nodes.
func isAssignment(call *ssa.Call, ctx *Context) bool {
	if call.Referrers() == nil {
		return false
	}

	for _, user := range *call.Referrers() {
		// Check for Phi nodes (merging control flow)
		// ALL Phi nodes represent assignments, regardless of location
		if _, ok := user.(*ssa.Phi); ok {
			return true
		}

		// Check for Store to Alloc (variable assignment)
		if store, ok := user.(*ssa.Store); ok {
			if _, ok := store.Addr.(*ssa.Alloc); ok {
				return true
			}
		}
	}

	return false
}

// Handle processes a Call instruction.
func (h *CallHandler) Handle(call *ssa.Call, ctx *Context) {
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

	// Find mutable root
	root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
	if root == nil {
		return // Immutable source
	}

	// KEY CHANGE: Process ALL calls uniformly (no isTerminal skip)
	// Record usage (violations detected later in DetectViolations)
	if isImmutableReturning {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, call.Block(), call.Pos())
	} else if isAssignment(call, ctx) {
		// Assignment creates new root - record but doesn't pollute
		ctx.Tracker.RecordAssignment(root, call.Block(), call.Pos())
	} else {
		// Actual use - pollutes the root
		ctx.Tracker.ProcessBranch(root, call.Block(), call.Pos())

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolationWithRoot(call.Pos(), root)
		}
	}

	// Check ALL possible roots for phi nodes
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv, ctx.LoopInfo)
	for _, r := range allRoots {
		if r == root {
			continue
		}
		if ctx.Tracker.IsPollutedAt(r, call.Block()) {
			ctx.Tracker.AddViolationWithRoot(call.Pos(), r)
		}
	}
}

// processBoundMethodCall handles calls through method values.
func (h *CallHandler) processBoundMethodCall(call *ssa.Call, mc *ssa.MakeClosure, isInLoop bool, ctx *Context) {
	if len(mc.Bindings) == 0 {
		return
	}

	recv := mc.Bindings[0]
	if !typeutil.IsGormDB(recv.Type()) {
		return
	}

	methodName := strings.TrimSuffix(mc.Fn.Name(), "$bound")
	isImmutableReturning := typeutil.IsImmutableReturningBuiltin(methodName)

	root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
	if root == nil {
		return
	}

	// Record usage (violations detected later)
	if isImmutableReturning {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, call.Block(), call.Pos())
	} else {
		// Non-pure methods pollute the root
		ctx.Tracker.ProcessBranch(root, call.Block(), call.Pos())

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolationWithRoot(call.Pos(), root)
		}
	}
}

// checkFunctionCallPollution handles non-gorm-method calls that take *gorm.DB.
func (h *CallHandler) checkFunctionCallPollution(call *ssa.Call, ctx *Context) {
	callee := call.Call.StaticCallee()

	if callee != nil {
		sig := callee.Signature
		if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
			return // This is a gorm method, not a function call
		}

		if ctx.RootTracer.IsPureFunction(callee) {
			return
		}
	}

	for _, arg := range call.Call.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg, ctx.LoopInfo)
		if root == nil {
			continue
		}

		// Mark polluted (function may use the value)
		ctx.Tracker.MarkPolluted(root, call.Block(), call.Pos())
	}
}

func (h *CallHandler) isGormDBMethodCall(call *ssa.Call) bool {
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

// GoHandler handles *ssa.Go instructions.
type GoHandler struct{}

// Handle processes a Go instruction.
func (h *GoHandler) Handle(g *ssa.Go, ctx *Context) {
	processGormDBCallCommon(&g.Call, g.Pos(), g.Block(), ctx)
}

// DeferHandler handles *ssa.Defer instructions.
type DeferHandler struct{}

// Handle processes a Defer instruction.
func (h *DeferHandler) Handle(d *ssa.Defer, ctx *Context) {
	processGormDBCallCommonDefer(&d.Call, d.Pos(), ctx)
}

// SendHandler handles *ssa.Send instructions.
type SendHandler struct{}

// Handle marks *gorm.DB sent to channels as polluted.
func (h *SendHandler) Handle(send *ssa.Send, ctx *Context) {
	if !typeutil.IsGormDB(send.X.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(send.X, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, send.Block(), send.Pos())
}

// StoreHandler handles *ssa.Store instructions.
type StoreHandler struct{}

// Handle marks *gorm.DB stored to slice elements as polluted.
func (h *StoreHandler) Handle(store *ssa.Store, ctx *Context) {
	if !typeutil.IsGormDB(store.Val.Type()) {
		return
	}

	// Only stores to IndexAddr (slice element)
	if _, ok := store.Addr.(*ssa.IndexAddr); !ok {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(store.Val, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, store.Block(), store.Pos())
}

// MapUpdateHandler handles *ssa.MapUpdate instructions.
type MapUpdateHandler struct{}

// Handle marks *gorm.DB stored in maps as polluted.
func (h *MapUpdateHandler) Handle(mapUpdate *ssa.MapUpdate, ctx *Context) {
	if !typeutil.IsGormDB(mapUpdate.Value.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(mapUpdate.Value, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mapUpdate.Block(), mapUpdate.Pos())
}

// MakeInterfaceHandler handles *ssa.MakeInterface instructions.
type MakeInterfaceHandler struct{}

// Handle marks *gorm.DB converted to interface as polluted.
func (h *MakeInterfaceHandler) Handle(mi *ssa.MakeInterface, ctx *Context) {
	if !typeutil.IsGormDB(mi.X.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(mi.X, ctx.LoopInfo)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mi.Block(), mi.Pos())
}

// processGormDBCallCommon processes gorm calls in go statements.
func processGormDBCallCommon(callCommon *ssa.CallCommon, pos token.Pos, block *ssa.BasicBlock, ctx *Context) {
	callee := callCommon.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature

	// Method call on *gorm.DB
	if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		if len(callCommon.Args) == 0 {
			return
		}
		recv := callCommon.Args[0]

		root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
		if root == nil {
			return
		}

		if ctx.Tracker.IsPollutedAt(root, block) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
		return
	}

	// Function call with *gorm.DB arguments
	for _, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg, ctx.LoopInfo)
		if root == nil {
			continue
		}

		if ctx.Tracker.IsPollutedAt(root, block) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
	}
}

// processGormDBCallCommonDefer processes gorm calls in defer statements.
func processGormDBCallCommonDefer(callCommon *ssa.CallCommon, pos token.Pos, ctx *Context) {
	callee := callCommon.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature

	// Method call on *gorm.DB
	if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		if len(callCommon.Args) == 0 {
			return
		}
		recv := callCommon.Args[0]

		root := ctx.RootTracer.FindMutableRoot(recv, ctx.LoopInfo)
		if root == nil {
			return
		}

		// Defer: check if polluted anywhere (executes at function exit)
		if ctx.Tracker.IsPollutedAnywhere(root) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
		return
	}

	// Function call with *gorm.DB arguments
	for _, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg, ctx.LoopInfo)
		if root == nil {
			continue
		}

		if ctx.Tracker.IsPollutedAnywhere(root) {
			ctx.Tracker.AddViolationWithRoot(pos, root)
		}
	}
}

// Dispatch dispatches an instruction to the appropriate handler.
func Dispatch(instr ssa.Instruction, ctx *Context) {
	switch i := instr.(type) {
	case *ssa.Call:
		(&CallHandler{}).Handle(i, ctx)
	case *ssa.Go:
		(&GoHandler{}).Handle(i, ctx)
	case *ssa.Send:
		(&SendHandler{}).Handle(i, ctx)
	case *ssa.Store:
		(&StoreHandler{}).Handle(i, ctx)
	case *ssa.MapUpdate:
		(&MapUpdateHandler{}).Handle(i, ctx)
	case *ssa.MakeInterface:
		(&MakeInterfaceHandler{}).Handle(i, ctx)
	}
}

// DispatchDefer handles defer instructions separately.
func DispatchDefer(d *ssa.Defer, ctx *Context) {
	(&DeferHandler{}).Handle(d, ctx)
}
