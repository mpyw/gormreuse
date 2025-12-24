// Package handler provides instruction handlers for gormreuse.
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
type Context struct {
	Tracker    *pollution.Tracker
	RootTracer *tracer.RootTracer
	CFG        *cfg.Analyzer
	LoopInfo   *cfg.LoopInfo
	CurrentFn  *ssa.Function
}

// CallHandler handles *ssa.Call instructions.
//
// Every gorm chain method use is processed uniformly:
// - First use from a root: OK, marks root polluted
// - Second+ use from same root: VIOLATION at that call site
type CallHandler struct{}

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
	isPureBuiltin := typeutil.IsPureFunctionBuiltin(methodName)

	// Get receiver
	if len(call.Call.Args) == 0 {
		return
	}
	recv := call.Call.Args[0]

	// Find mutable root
	root := ctx.RootTracer.FindMutableRoot(recv)
	if root == nil {
		return // Immutable source
	}

	// KEY CHANGE: Process ALL calls uniformly (no isTerminal skip)
	// Record usage (violations detected later in DetectViolations)
	if isPureBuiltin {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, call.Block(), call.Pos())
	} else {
		// Non-pure methods pollute the root
		ctx.Tracker.ProcessBranch(root, call.Block(), call.Pos())

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolation(call.Pos())
		}
	}

	// Check ALL possible roots for phi nodes
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv)
	for _, r := range allRoots {
		if r == root {
			continue
		}
		if ctx.Tracker.IsPollutedAt(r, call.Block()) {
			ctx.Tracker.AddViolation(call.Pos())
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
	isPureBuiltin := typeutil.IsPureFunctionBuiltin(methodName)

	root := ctx.RootTracer.FindMutableRoot(recv)
	if root == nil {
		return
	}

	// Record usage (violations detected later)
	if isPureBuiltin {
		// Pure methods check for pollution but don't pollute
		ctx.Tracker.RecordPureUse(root, call.Block(), call.Pos())
	} else {
		// Non-pure methods pollute the root
		ctx.Tracker.ProcessBranch(root, call.Block(), call.Pos())

		// Loop with external root - immediate violation (only for non-pure methods)
		if isInLoop && ctx.CFG.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolation(call.Pos())
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

		root := ctx.RootTracer.FindMutableRoot(arg)
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

	root := ctx.RootTracer.FindMutableRoot(send.X)
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

	root := ctx.RootTracer.FindMutableRoot(store.Val)
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

	root := ctx.RootTracer.FindMutableRoot(mapUpdate.Value)
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

	root := ctx.RootTracer.FindMutableRoot(mi.X)
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

		root := ctx.RootTracer.FindMutableRoot(recv)
		if root == nil {
			return
		}

		if ctx.Tracker.IsPollutedAt(root, block) {
			ctx.Tracker.AddViolation(pos)
		}
		return
	}

	// Function call with *gorm.DB arguments
	for _, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg)
		if root == nil {
			continue
		}

		if ctx.Tracker.IsPollutedAt(root, block) {
			ctx.Tracker.AddViolation(pos)
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

		root := ctx.RootTracer.FindMutableRoot(recv)
		if root == nil {
			return
		}

		// Defer: check if polluted anywhere (executes at function exit)
		if ctx.Tracker.IsPollutedAnywhere(root) {
			ctx.Tracker.AddViolation(pos)
		}
		return
	}

	// Function call with *gorm.DB arguments
	for _, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg)
		if root == nil {
			continue
		}

		if ctx.Tracker.IsPollutedAnywhere(root) {
			ctx.Tracker.AddViolation(pos)
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
