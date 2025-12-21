// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Instruction Handlers
//
// This file contains methods for handling different SSA instruction types.
// Each handler processes a specific instruction type and updates pollution state.
// =============================================================================

// processCall handles a regular *ssa.Call instruction.
func (a *usageAnalyzer) processCall(call *ssa.Call, isInLoop bool, loopBlocks map[*ssa.BasicBlock]bool) {

	// Check if this is a function call that takes *gorm.DB as argument (pollutes it)
	a.checkFunctionCallPollution(call)

	// Check if this is a bound method call (method value)
	// e.g., find := q.Find; find(nil)
	// In this case, the receiver is in MakeClosure.Bindings[0], not in Args[0]
	if mc, ok := call.Call.Value.(*ssa.MakeClosure); ok {
		a.processBoundMethodCall(call, mc, isInLoop, loopBlocks)
		return
	}

	// Check if this is a method call on *gorm.DB
	callee := call.Call.StaticCallee()
	if !a.isGormDBMethodCall(call) {
		return
	}

	if callee == nil {
		return
	}

	methodName := callee.Name()
	isSafeMethod := IsSafeMethod(methodName)
	isTerminal := a.isTerminalCall(call)

	// Skip non-terminal Chain Methods (part of chain construction)
	// But process Safe Methods even if non-terminal (to detect polluted receiver)
	if !isTerminal && !isSafeMethod {
		return
	}

	// Get the receiver
	if len(call.Call.Args) == 0 {
		return
	}
	recv := call.Call.Args[0]

	// Find the mutable root being used (for state tracking)
	root := a.findMutableRoot(recv)
	if root == nil {
		return // Receiver is immutable (Session result, parameter, etc.)
	}

	// Get or create state for this root
	state := a.getOrCreateState(root)
	currentBlock := call.Block()

	// Check for same-block pollution (inline checking is correct for same block)
	// This handles sequential calls like: q.Find(); q.Count()
	if _, pollutedInSameBlock := state.pollutedBlocks[currentBlock]; pollutedInSameBlock {
		// Already polluted in the same block - this is a violation
		state.violations = append(state.violations, call.Pos())
	} else if !isSafeMethod && isTerminal {
		// Terminal Chain Method - mark as polluted
		state.pollutedBlocks[currentBlock] = call.Pos()

		// If in a loop AND the root is defined outside the loop, report as violation
		// (second iteration will reuse the polluted root)
		if isInLoop && a.isRootDefinedOutsideLoop(root, loopBlocks) {
			state.violations = append(state.violations, call.Pos())
		}
	}

	// Check ALL possible roots for pollution (handles Phi nodes from switch/if).
	// If ANY path leads to a polluted root, it's a violation.
	// This handles cases like: switch { case 1: q = fresh; default: /* q still polluted */ }
	allRoots := a.findAllMutableRoots(recv)
	for _, r := range allRoots {
		if r == root {
			continue // Already checked above
		}
		otherState := a.states[r]
		if otherState != nil && a.isPollutedAt(otherState, currentBlock) {
			otherState.violations = append(otherState.violations, call.Pos())
		}
	}
}

// processBoundMethodCall handles a bound method call (method value).
// When a method is extracted as a value (e.g., find := q.Find), calling it
// (find(nil)) has the receiver in MakeClosure.Bindings[0], not in Args[0].
func (a *usageAnalyzer) processBoundMethodCall(call *ssa.Call, mc *ssa.MakeClosure, isInLoop bool, loopBlocks map[*ssa.BasicBlock]bool) {
	// For bound methods, the receiver is in Bindings[0], not in the signature
	// Check if this is a *gorm.DB bound method by checking Bindings[0] type
	if len(mc.Bindings) == 0 {
		return
	}

	recv := mc.Bindings[0]
	if !IsGormDB(recv.Type()) {
		return
	}

	// Get method name from bound function (strip $bound suffix)
	methodName := strings.TrimSuffix(mc.Fn.Name(), "$bound")

	isSafeMethod := IsSafeMethod(methodName)
	isTerminal := a.isTerminalCall(call)

	// Skip non-terminal Chain Methods
	if !isTerminal && !isSafeMethod {
		return
	}

	// Find the mutable root being used
	root := a.findMutableRoot(recv)
	if root == nil {
		return // Receiver is immutable
	}

	// Get or create state for this root
	state := a.getOrCreateState(root)
	currentBlock := call.Block()

	// Check for same-block pollution
	if _, pollutedInSameBlock := state.pollutedBlocks[currentBlock]; pollutedInSameBlock {
		state.violations = append(state.violations, call.Pos())
	} else if !isSafeMethod && isTerminal {
		// Terminal Chain Method - mark as polluted
		state.pollutedBlocks[currentBlock] = call.Pos()

		if isInLoop && a.isRootDefinedOutsideLoop(root, loopBlocks) {
			state.violations = append(state.violations, call.Pos())
		}
	}
}

// =============================================================================
// Defer and Go Statement Handlers
// =============================================================================

// processDeferStatement handles a defer statement (second pass).
// Defers execute at function exit, so we check for any pollution in the function.
func (a *usageAnalyzer) processDeferStatement(d *ssa.Defer) {
	a.processCallCommonForDefer(&d.Call, d.Pos(), d.Parent())
}

// processGoStatement handles a go statement.
func (a *usageAnalyzer) processGoStatement(g *ssa.Go) {
	a.processCallCommonForGo(&g.Call, g.Pos(), g.Block())
}

// processCallCommonForDefer handles CallCommon from Defer instructions.
// For defer: executed at function exit, so any pollution in the function affects it.
func (a *usageAnalyzer) processCallCommonForDefer(callCommon *ssa.CallCommon, pos token.Pos, fn *ssa.Function) {
	callee := callCommon.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature

	// Check if this is a *gorm.DB method call
	if sig != nil && sig.Recv() != nil && IsGormDB(sig.Recv().Type()) {
		// Get the receiver
		if len(callCommon.Args) == 0 {
			return
		}
		recv := callCommon.Args[0]

		// Find the mutable root being used
		root := a.findMutableRoot(recv)
		if root == nil {
			return // Receiver is immutable
		}

		// Get or create state for this root
		state := a.getOrCreateState(root)

		// Check if root was polluted anywhere (defer executes at function exit)
		if a.isPollutedAnywhere(state, fn) {
			// Using a polluted value - violation!
			state.violations = append(state.violations, pos)
		}
		return
	}

	// Check if this is a function call that takes *gorm.DB as argument
	for _, arg := range callCommon.Args {
		if !IsGormDB(arg.Type()) {
			continue
		}

		root := a.findMutableRoot(arg)
		if root == nil {
			continue
		}

		state := a.getOrCreateState(root)

		// For defer, check if polluted anywhere
		if a.isPollutedAnywhere(state, fn) {
			state.violations = append(state.violations, pos)
		}
	}
}

// processCallCommonForGo handles CallCommon from Go instructions.
// For go: executed concurrently, use block-aware pollution check.
func (a *usageAnalyzer) processCallCommonForGo(callCommon *ssa.CallCommon, pos token.Pos, block *ssa.BasicBlock) {
	callee := callCommon.StaticCallee()
	if callee == nil {
		return
	}

	sig := callee.Signature

	// Check if this is a *gorm.DB method call
	if sig != nil && sig.Recv() != nil && IsGormDB(sig.Recv().Type()) {
		// Get the receiver
		if len(callCommon.Args) == 0 {
			return
		}
		recv := callCommon.Args[0]

		// Find the mutable root being used
		root := a.findMutableRoot(recv)
		if root == nil {
			return // Receiver is immutable
		}

		// Get or create state for this root
		state := a.getOrCreateState(root)

		// Check if root was polluted (from a reachable block)
		if a.isPollutedAt(state, block) {
			// Using a polluted value - violation!
			state.violations = append(state.violations, pos)
		}
		return
	}

	// Check if this is a function call that takes *gorm.DB as argument
	for _, arg := range callCommon.Args {
		if !IsGormDB(arg.Type()) {
			continue
		}

		root := a.findMutableRoot(arg)
		if root == nil {
			continue
		}

		state := a.getOrCreateState(root)

		// For go, check if polluted from reachable block
		if a.isPollutedAt(state, block) {
			state.violations = append(state.violations, pos)
		}
	}
}

// =============================================================================
// Storage Operation Handlers
// =============================================================================

// processSend handles channel send operations.
// If sending *gorm.DB to a channel, we mark the value as polluted
// (it may be received and used elsewhere, causing pollution).
func (a *usageAnalyzer) processSend(send *ssa.Send) {
	if !IsGormDB(send.X.Type()) {
		return
	}

	root := a.findMutableRoot(send.X)
	if root == nil {
		return
	}

	// Mark as polluted (channel send assumed to pollute)
	a.markPolluted(root, send.Block(), send.Pos())
}

// processStore handles store operations to slice elements.
// If storing *gorm.DB to a slice element, we mark the value as polluted.
// Note: FieldAddr (struct fields) are handled via traceFieldStore instead,
// which allows proper tracking of field access patterns.
func (a *usageAnalyzer) processStore(store *ssa.Store) {
	if !IsGormDB(store.Val.Type()) {
		return
	}

	// Only process stores to IndexAddr (slice element)
	// Skip stores to Alloc (local variable assignment, closure capture, etc.)
	// Skip stores to FieldAddr (struct field) - handled via tracing
	switch store.Addr.(type) {
	case *ssa.IndexAddr:
		// Stores to slice elements - mark as polluted (can't track index access)
	default:
		return
	}

	root := a.findMutableRoot(store.Val)
	if root == nil {
		return
	}

	// Mark as polluted (slice element store assumed to pollute)
	a.markPolluted(root, store.Block(), store.Pos())
}

// processMapUpdate handles map update operations.
// If storing *gorm.DB in a map, we mark the value as polluted.
func (a *usageAnalyzer) processMapUpdate(mapUpdate *ssa.MapUpdate) {
	if !IsGormDB(mapUpdate.Value.Type()) {
		return
	}

	root := a.findMutableRoot(mapUpdate.Value)
	if root == nil {
		return
	}

	// Mark as polluted (map update assumed to pollute)
	a.markPolluted(root, mapUpdate.Block(), mapUpdate.Pos())
}

// processMakeInterface handles conversion of *gorm.DB to interface{}.
// This is polluting because the value may be extracted via type assertion.
func (a *usageAnalyzer) processMakeInterface(mi *ssa.MakeInterface) {
	if !IsGormDB(mi.X.Type()) {
		return
	}

	root := a.findMutableRoot(mi.X)
	if root == nil {
		return
	}

	// Mark as polluted (interface conversion assumed to pollute)
	a.markPolluted(root, mi.Block(), mi.Pos())
}

// =============================================================================
// Pollution State Checks
// =============================================================================

// isPollutedAt checks if the value is polluted at the given block.
// A value is polluted at block B if there exists a polluted block A
// such that A can reach B (there is a path from A to B in the CFG).
func (a *usageAnalyzer) isPollutedAt(state *valueState, targetBlock *ssa.BasicBlock) bool {
	if state == nil || len(state.pollutedBlocks) == 0 {
		return false
	}
	if targetBlock == nil {
		return false
	}
	for pollutedBlock := range state.pollutedBlocks {
		if pollutedBlock == nil {
			continue
		}
		// If pollution is from a different function (closure), conservatively consider it reachable
		if pollutedBlock.Parent() != targetBlock.Parent() {
			return true
		}
		// Same function: check if polluted block can reach target block
		if a.canReach(pollutedBlock, targetBlock) {
			return true
		}
	}
	return false
}

// isPollutedAnywhere checks if the value is polluted anywhere in the given function.
// Used for defer statements which execute at function exit.
func (a *usageAnalyzer) isPollutedAnywhere(state *valueState, fn *ssa.Function) bool {
	if state == nil || len(state.pollutedBlocks) == 0 {
		return false
	}
	for pollutedBlock := range state.pollutedBlocks {
		if pollutedBlock == nil {
			continue
		}
		// Check if this pollution is from the same function or a closure of it
		if pollutedBlock.Parent() == fn {
			return true
		}
		// Also check closures (parent function captures the value)
		// Closure pollution affects the parent function's defers
		return true
	}
	return false
}

// =============================================================================
// Function Call Helpers
// =============================================================================

// checkFunctionCallPollution checks if a function call takes *gorm.DB as argument.
// If so, we assume the function pollutes the *gorm.DB (unless marked pure).
func (a *usageAnalyzer) checkFunctionCallPollution(call *ssa.Call) {
	callee := call.Call.StaticCallee()

	// Handle static calls
	if callee != nil {
		// Skip *gorm.DB method calls - handled separately
		sig := callee.Signature
		if sig != nil && sig.Recv() != nil && IsGormDB(sig.Recv().Type()) {
			return
		}

		// Check if function is marked as pure
		if a.isPureFunction(callee) {
			return
		}
	}

	// For interface method calls (StaticCallee == nil), we assume pollution
	// unless it's a *gorm.DB method call (which is handled separately)

	// Check each argument for *gorm.DB
	for _, arg := range call.Call.Args {
		if !IsGormDB(arg.Type()) {
			continue
		}

		// Find the mutable root for this argument
		root := a.findMutableRoot(arg)
		if root == nil {
			continue // Immutable
		}

		// Mark as polluted (function call assumed to pollute)
		a.markPolluted(root, call.Block(), call.Pos())
	}
}

// isPureFunction checks if a function is marked as pure.
func (a *usageAnalyzer) isPureFunction(fn *ssa.Function) bool {
	if a.pureFuncs == nil {
		return false
	}

	// Build function name: pkgPath.FuncName or pkgPath.(ReceiverType).MethodName
	fullName := fn.String()
	_, exists := a.pureFuncs[fullName]
	return exists
}

// =============================================================================
// Call Classification Helpers
// =============================================================================

// closureCapturesGormDB checks if a MakeClosure captures any *gorm.DB values.
// Note: Go closures capture by reference, so bindings may be **gorm.DB (pointer to the variable).
func (a *usageAnalyzer) closureCapturesGormDB(mc *ssa.MakeClosure) bool {
	for _, binding := range mc.Bindings {
		t := binding.Type()
		// Check for *gorm.DB
		if IsGormDB(t) {
			return true
		}
		// Check for **gorm.DB (captured by reference)
		if ptr, ok := t.(*types.Pointer); ok {
			if IsGormDB(ptr.Elem()) {
				return true
			}
		}
	}
	return false
}

// isTerminalCall checks if this call's result is NOT used as receiver in another chain call.
func (a *usageAnalyzer) isTerminalCall(call *ssa.Call) bool {
	// If result is not *gorm.DB, it's terminal (e.g., Count returns int64)
	if !IsGormDB(call.Type()) {
		return true
	}

	// Check referrers
	refs := call.Referrers()
	if refs == nil || len(*refs) == 0 {
		// Result is discarded
		return true
	}

	// Check if any referrer uses this as receiver in a *gorm.DB method call
	for _, ref := range *refs {
		refCall, ok := ref.(*ssa.Call)
		if !ok {
			continue
		}
		if a.isGormDBMethodCall(refCall) && len(refCall.Call.Args) > 0 && refCall.Call.Args[0] == call {
			// Used as receiver in another chain call - NOT terminal
			return false
		}
	}

	// Result is stored in variable or used elsewhere (not in chain)
	return true
}

// isGormDBMethodCall checks if this is a method call on *gorm.DB.
func (a *usageAnalyzer) isGormDBMethodCall(call *ssa.Call) bool {
	callee := call.Call.StaticCallee()
	if callee == nil {
		return false
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil {
		return false
	}

	return IsGormDB(sig.Recv().Type())
}
