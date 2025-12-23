package ssa

import (
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// InstructionHandler Interface (Strategy Pattern)
//
// Each handler is responsible for processing a specific type of SSA instruction.
// The analyzer iterates through instructions and delegates to appropriate handlers.
// =============================================================================

// HandlerContext provides shared context for instruction handlers.
type HandlerContext struct {
	Tracker     *PollutionTracker
	RootTracer  *RootTracer
	CFGAnalyzer *CFGAnalyzer
	LoopInfo    *LoopInfo
	CurrentFn   *ssa.Function
}

// InstructionHandler handles a specific type of SSA instruction.
type InstructionHandler interface {
	// CanHandle returns true if this handler can process the given instruction.
	CanHandle(instr ssa.Instruction) bool

	// Handle processes the instruction and updates the analysis context.
	Handle(instr ssa.Instruction, ctx *HandlerContext)
}

// =============================================================================
// Handler Implementations
// =============================================================================

// CallHandler handles *ssa.Call instructions.
//
// This is the primary handler for detecting *gorm.DB reuse violations.
// It processes method calls on *gorm.DB and function calls that accept *gorm.DB.
type CallHandler struct{}

// CanHandle returns true for Call instructions.
func (h *CallHandler) CanHandle(instr ssa.Instruction) bool {
	_, ok := instr.(*ssa.Call)
	return ok
}

// Handle processes a Call instruction for pollution detection.
//
// Example scenarios:
//
//	Scenario 1: Terminal chain method (pollutes the root)
//	  q := db.Where("x")     // q is mutable root
//	  q.Find(nil)            // Terminal call → marks q as polluted
//	  q.Find(nil)            // Violation! q is already polluted
//
//	Scenario 2: Bound method (method value)
//	  q := db.Where("x")
//	  find := q.Find         // MakeClosure with receiver bound
//	  find(nil)              // Call through bound method → pollutes q
//
//	Scenario 3: Helper function pollutes argument
//	  q := db.Where("x")
//	  doSomething(q)         // Non-pure function → marks q as polluted
//
//	Scenario 4: Loop with external root
//	  q := db.Where("x")
//	  for i := range items {
//	      q.Find(nil)        // In loop + root outside → immediate violation
//	  }
func (h *CallHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	call := instr.(*ssa.Call)
	isInLoop := ctx.LoopInfo.IsInLoop(call.Block())

	// Check if this is a function call that takes *gorm.DB as argument (pollutes it)
	h.checkFunctionCallPollution(call, ctx)

	// Check if this is a bound method call (method value)
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		h.processBoundMethodCall(call, mc, isInLoop, ctx)
		return
	}

	// Check if this is a method call on *gorm.DB
	callee := call.Call.StaticCallee()
	if !h.isGormDBMethodCall(call) {
		return
	}

	if callee == nil {
		return
	}

	methodName := callee.Name()
	isPureBuiltin := typeutil.IsPureFunctionBuiltin(methodName)
	isTerminal := h.isTerminalCall(call)

	// Skip non-terminal Chain Methods (part of chain construction)
	if !isTerminal && !isPureBuiltin {
		return
	}

	// Get the receiver
	if len(call.Call.Args) == 0 {
		return
	}
	recv := call.Call.Args[0]

	// Find the mutable root being used
	root := ctx.RootTracer.FindMutableRoot(recv)
	if root == nil {
		return // Receiver is immutable
	}

	currentBlock := call.Block()

	// Check for same-block pollution
	if ctx.Tracker.IsPollutedInBlock(root, currentBlock) {
		ctx.Tracker.AddViolation(root, call.Pos())
	} else if !isPureBuiltin && isTerminal {
		// Terminal Chain Method - mark as polluted
		ctx.Tracker.MarkPolluted(root, currentBlock, call.Pos())

		// If in a loop AND the root is defined outside the loop, report as violation
		if isInLoop && ctx.CFGAnalyzer.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolation(root, call.Pos())
		}
	}

	// Check ALL possible roots for pollution (handles Phi nodes from switch/if)
	allRoots := ctx.RootTracer.FindAllMutableRoots(recv)
	for _, r := range allRoots {
		if r == root {
			continue
		}
		if ctx.Tracker.IsPollutedAt(r, currentBlock) {
			ctx.Tracker.AddViolation(r, call.Pos())
		}
	}
}

// processBoundMethodCall handles calls through method values (bound methods).
//
// Example:
//
//	q := db.Where("x")
//	find := q.Find          // MakeClosure: Fn=(*gorm.DB).Find$bound, Bindings=[q]
//	find(&users)            // Call: Value=MakeClosure
//
//	SSA representation:
//	  t0 = db.Where("x")              // q
//	  t1 = make closure (*gorm.DB).Find$bound [t0]  // find
//	  t2 = t1(&users)                 // call through bound method
//
//	Detection:
//	  1. mc.Bindings[0] is the receiver (q)
//	  2. Method name has "$bound" suffix
//	  3. Apply same terminal/pollution logic as direct calls
func (h *CallHandler) processBoundMethodCall(call *ssa.Call, mc *ssa.MakeClosure, isInLoop bool, ctx *HandlerContext) {
	if len(mc.Bindings) == 0 {
		return
	}

	recv := mc.Bindings[0]
	if !typeutil.IsGormDB(recv.Type()) {
		return
	}

	methodName := strings.TrimSuffix(mc.Fn.Name(), "$bound")
	isPureBuiltin := typeutil.IsPureFunctionBuiltin(methodName)
	isTerminal := h.isTerminalCall(call)

	if !isTerminal && !isPureBuiltin {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(recv)
	if root == nil {
		return
	}

	currentBlock := call.Block()

	if ctx.Tracker.IsPollutedInBlock(root, currentBlock) {
		ctx.Tracker.AddViolation(root, call.Pos())
	} else if !isPureBuiltin && isTerminal {
		ctx.Tracker.MarkPolluted(root, currentBlock, call.Pos())

		if isInLoop && ctx.CFGAnalyzer.IsDefinedOutsideLoop(root, ctx.LoopInfo) {
			ctx.Tracker.AddViolation(root, call.Pos())
		}
	}
}

func (h *CallHandler) checkFunctionCallPollution(call *ssa.Call, ctx *HandlerContext) {
	callee := call.Call.StaticCallee()

	if callee != nil {
		sig := callee.Signature
		if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
			return
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

		ctx.Tracker.MarkPolluted(root, call.Block(), call.Pos())
	}
}

// isTerminalCall determines if a call consumes the chain (vs continues building it).
//
// Example:
//
//	Terminal calls (result not used as receiver):
//	  q.Find(&users)                  // Returns *gorm.DB but not used for chaining
//	  q.Where("x")                    // Result is not used at all
//	  q.Where("x").Find(&users)       // Find is terminal, Where is not
//
//	Non-terminal calls (result used as receiver):
//	  q.Where("x").Where("y")         // First Where's result is receiver of second
//	  q.Joins("...").Preload("...")   // Joins result is receiver of Preload
//
//	Detection logic:
//	  1. If return type is not *gorm.DB → terminal
//	  2. If no referrers → terminal
//	  3. If any referrer uses this as receiver for *gorm.DB method → non-terminal
func (h *CallHandler) isTerminalCall(call *ssa.Call) bool {
	if !typeutil.IsGormDB(call.Type()) {
		return true
	}

	refs := call.Referrers()
	if refs == nil || len(*refs) == 0 {
		return true
	}

	for _, ref := range *refs {
		refCall, ok := ref.(*ssa.Call)
		if !ok {
			continue
		}
		if h.isGormDBMethodCall(refCall) && len(refCall.Call.Args) > 0 && refCall.Call.Args[0] == call {
			return false
		}
	}

	return true
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

// =============================================================================
// GoHandler handles *ssa.Go instructions.
// =============================================================================

// GoHandler handles goroutine spawning instructions.
type GoHandler struct{}

// CanHandle returns true for Go instructions.
func (h *GoHandler) CanHandle(instr ssa.Instruction) bool {
	_, ok := instr.(*ssa.Go)
	return ok
}

// Handle processes a Go instruction for pollution detection.
func (h *GoHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	g := instr.(*ssa.Go)
	h.processCallCommon(&g.Call, g.Pos(), g.Block(), ctx)
}

func (h *GoHandler) processCallCommon(callCommon *ssa.CallCommon, pos token.Pos, block *ssa.BasicBlock, ctx *HandlerContext) {
	callee := callCommon.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature

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
			ctx.Tracker.AddViolation(root, pos)
		}
		return
	}

	for _, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg)
		if root == nil {
			continue
		}

		if ctx.Tracker.IsPollutedAt(root, block) {
			ctx.Tracker.AddViolation(root, pos)
		}
	}
}

// =============================================================================
// DeferHandler handles *ssa.Defer instructions.
//
// Deferred calls execute at function exit, so they see pollution from ANY block.
// This is different from regular calls which only see pollution from reachable blocks.
//
// Example:
//
//	func example(db *gorm.DB) {
//	    q := db.Where("x")
//	    defer q.Find(nil)       // Deferred: will execute at function exit
//	    q.Find(nil)             // Pollutes q
//	}                           // defer executes here → violation!
//
// Detection:
//
//	Unlike regular calls, defer checks IsPollutedAnywhere (not IsPollutedAt)
//	because the deferred call executes after all other code in the function.
// =============================================================================

// DeferHandler handles deferred call instructions.
type DeferHandler struct{}

// CanHandle returns true for Defer instructions.
func (h *DeferHandler) CanHandle(instr ssa.Instruction) bool {
	_, ok := instr.(*ssa.Defer)
	return ok
}

// Handle processes a Defer instruction for pollution detection.
func (h *DeferHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	d := instr.(*ssa.Defer)
	h.processCallCommon(&d.Call, d.Pos(), d.Parent(), ctx)
}

func (h *DeferHandler) processCallCommon(callCommon *ssa.CallCommon, pos token.Pos, fn *ssa.Function, ctx *HandlerContext) {
	callee := callCommon.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature

	if sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		if len(callCommon.Args) == 0 {
			return
		}
		recv := callCommon.Args[0]

		root := ctx.RootTracer.FindMutableRoot(recv)
		if root == nil {
			return
		}

		if ctx.Tracker.IsPollutedAnywhere(root, fn) {
			ctx.Tracker.AddViolation(root, pos)
		}
		return
	}

	for _, arg := range callCommon.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		root := ctx.RootTracer.FindMutableRoot(arg)
		if root == nil {
			continue
		}

		if ctx.Tracker.IsPollutedAnywhere(root, fn) {
			ctx.Tracker.AddViolation(root, pos)
		}
	}
}

// =============================================================================
// SendHandler handles *ssa.Send instructions (channel send).
//
// Sending *gorm.DB through a channel is treated as pollution because:
//   - The receiver goroutine might use it concurrently
//   - We can't track usage across goroutines statically
//
// Example:
//
//	q := db.Where("x")
//	ch <- q                 // Send pollutes q
//	q.Find(nil)             // Violation! q was sent to channel
// =============================================================================

// SendHandler handles channel send instructions.
type SendHandler struct{}

// CanHandle returns true for Send instructions.
func (h *SendHandler) CanHandle(instr ssa.Instruction) bool {
	_, ok := instr.(*ssa.Send)
	return ok
}

// Handle marks *gorm.DB values sent to channels as polluted.
func (h *SendHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	send := instr.(*ssa.Send)

	if !typeutil.IsGormDB(send.X.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(send.X)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, send.Block(), send.Pos())
}

// =============================================================================
// StoreHandler handles *ssa.Store instructions.
//
// Stores to slice/array elements are treated as pollution because:
//   - The slice might be shared with other code
//   - We can't track which element is accessed
//
// Example:
//
//	q := db.Where("x")
//	dbs := make([]*gorm.DB, 1)
//	dbs[0] = q              // Store to IndexAddr → pollutes q
//	q.Find(nil)             // Violation! q was stored in slice
//
// Note: Simple stores to local variables (Alloc) are NOT pollution.
//
//	They're handled by SSATracer for value tracking.
// =============================================================================

// StoreHandler handles store instructions.
type StoreHandler struct{}

// CanHandle returns true for Store instructions.
func (h *StoreHandler) CanHandle(instr ssa.Instruction) bool {
	_, ok := instr.(*ssa.Store)
	return ok
}

// Handle marks *gorm.DB values stored to slice elements as polluted.
func (h *StoreHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	store := instr.(*ssa.Store)

	if !typeutil.IsGormDB(store.Val.Type()) {
		return
	}

	// Only process stores to IndexAddr (slice element)
	switch store.Addr.(type) {
	case *ssa.IndexAddr:
		// Stores to slice elements - mark as polluted
	default:
		return
	}

	root := ctx.RootTracer.FindMutableRoot(store.Val)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, store.Block(), store.Pos())
}

// =============================================================================
// MapUpdateHandler handles *ssa.MapUpdate instructions.
//
// Storing *gorm.DB in a map is treated as pollution because:
//   - The map might be shared with other code
//   - We can't track which key is accessed or when
//
// Example:
//
//	q := db.Where("x")
//	m := make(map[string]*gorm.DB)
//	m["key"] = q            // MapUpdate → pollutes q
//	q.Find(nil)             // Violation! q was stored in map
// =============================================================================

// MapUpdateHandler handles map update instructions.
type MapUpdateHandler struct{}

// CanHandle returns true for MapUpdate instructions.
func (h *MapUpdateHandler) CanHandle(instr ssa.Instruction) bool {
	_, ok := instr.(*ssa.MapUpdate)
	return ok
}

// Handle marks *gorm.DB values stored in maps as polluted.
func (h *MapUpdateHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	mapUpdate := instr.(*ssa.MapUpdate)

	if !typeutil.IsGormDB(mapUpdate.Value.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(mapUpdate.Value)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mapUpdate.Block(), mapUpdate.Pos())
}

// =============================================================================
// MakeInterfaceHandler handles *ssa.MakeInterface instructions.
//
// Converting *gorm.DB to interface{} is treated as pollution because:
//   - Type assertion might extract it later
//   - We can't track usage through interface types
//
// Example:
//
//	q := db.Where("x")
//	var i interface{} = q   // MakeInterface → pollutes q
//	q.Find(nil)             // Violation! q was wrapped in interface
// =============================================================================

// MakeInterfaceHandler handles interface conversion instructions.
type MakeInterfaceHandler struct{}

// CanHandle returns true for MakeInterface instructions.
func (h *MakeInterfaceHandler) CanHandle(instr ssa.Instruction) bool {
	_, ok := instr.(*ssa.MakeInterface)
	return ok
}

// Handle marks *gorm.DB values converted to interfaces as polluted.
func (h *MakeInterfaceHandler) Handle(instr ssa.Instruction, ctx *HandlerContext) {
	mi := instr.(*ssa.MakeInterface)

	if !typeutil.IsGormDB(mi.X.Type()) {
		return
	}

	root := ctx.RootTracer.FindMutableRoot(mi.X)
	if root == nil {
		return
	}

	ctx.Tracker.MarkPolluted(root, mi.Block(), mi.Pos())
}

// =============================================================================
// Helper Functions
// =============================================================================

// ClosureCapturesGormDB checks if a MakeClosure captures any *gorm.DB values.
func ClosureCapturesGormDB(mc *ssa.MakeClosure) bool {
	for _, binding := range mc.Bindings {
		t := binding.Type()
		if typeutil.IsGormDB(t) {
			return true
		}
		if ptr, ok := t.(*types.Pointer); ok {
			if typeutil.IsGormDB(ptr.Elem()) {
				return true
			}
		}
	}
	return false
}

// DefaultHandlers returns the default set of instruction handlers.
func DefaultHandlers() []InstructionHandler {
	return []InstructionHandler{
		&CallHandler{},
		&GoHandler{},
		&SendHandler{},
		&StoreHandler{},
		&MapUpdateHandler{},
		&MakeInterfaceHandler{},
		// Note: DeferHandler is not included here because defers are processed
		// in a second pass after all regular instructions.
	}
}
