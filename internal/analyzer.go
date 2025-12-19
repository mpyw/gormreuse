// Package internal provides SSA-based analysis for GORM *gorm.DB reuse detection.
package internal

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Entry Point
// =============================================================================

// RunSSA performs SSA-based analysis for GORM *gorm.DB reuse detection.
func RunSSA(
	pass *analysis.Pass,
	ssaInfo *buildssa.SSA,
	ignoreMaps map[string]IgnoreMap,
	funcIgnores map[string]map[token.Pos]struct{},
	pureFuncs map[string]struct{},
) {
	for _, fn := range ssaInfo.SrcFuncs {
		pos := fn.Pos()
		if !pos.IsValid() {
			continue
		}

		filename := pass.Fset.Position(pos).Filename
		ignoreMap := ignoreMaps[filename]

		// Check if entire function is ignored
		if funcIgnoreSet, ok := funcIgnores[filename]; ok {
			if _, ignored := funcIgnoreSet[fn.Pos()]; ignored {
				continue
			}
		}

		chk := newChecker(pass, ignoreMap, pureFuncs)
		chk.checkFunction(fn)
	}
}

// =============================================================================
// SSA Checker
// =============================================================================

type checker struct {
	pass      *analysis.Pass
	ignoreMap IgnoreMap
	pureFuncs map[string]struct{}
	reported  map[token.Pos]bool
}

func newChecker(pass *analysis.Pass, ignoreMap IgnoreMap, pureFuncs map[string]struct{}) *checker {
	return &checker{
		pass:      pass,
		ignoreMap: ignoreMap,
		pureFuncs: pureFuncs,
		reported:  make(map[token.Pos]bool),
	}
}

func (c *checker) checkFunction(fn *ssa.Function) {
	analyzer := newUsageAnalyzer(fn, c.pureFuncs)
	violations := analyzer.analyze()

	for _, v := range violations {
		c.report(v.pos, v.message)
	}
}

func (c *checker) report(pos token.Pos, message string) {
	if c.reported[pos] {
		return
	}
	c.reported[pos] = true

	line := c.pass.Fset.Position(pos).Line
	if c.ignoreMap != nil && c.ignoreMap.ShouldIgnore(line) {
		return
	}

	c.pass.Reportf(pos, "%s", message)
}

// =============================================================================
// Usage Analyzer
// =============================================================================

type violation struct {
	pos     token.Pos
	message string
}

// valueState tracks the state of a *gorm.DB value.
type valueState struct {
	// isPolluted indicates this mutable value has been used with a Chain Method.
	isPolluted bool

	// pollutedAt is the position where this value was first polluted.
	pollutedAt token.Pos

	// violations tracks positions where polluted value was reused.
	violations []token.Pos
}

type usageAnalyzer struct {
	fn *ssa.Function

	// states maps each tracked *gorm.DB chain root to its state.
	states map[ssa.Value]*valueState

	// pureFuncs is a set of functions marked as pure (don't pollute *gorm.DB).
	pureFuncs map[string]struct{}
}

func newUsageAnalyzer(fn *ssa.Function, pureFuncs map[string]struct{}) *usageAnalyzer {
	return &usageAnalyzer{
		fn:        fn,
		states:    make(map[ssa.Value]*valueState),
		pureFuncs: pureFuncs,
	}
}

func (a *usageAnalyzer) analyze() []violation {
	// Process all *gorm.DB method calls and track pollution
	// This includes method calls in closures that capture tracked values
	a.processMethodCalls(a.fn, make(map[*ssa.Function]bool))

	// Collect violations
	return a.collectViolations()
}

// processMethodCalls processes all *gorm.DB method calls to track pollution.
// It also recursively processes closures that capture *gorm.DB values.
func (a *usageAnalyzer) processMethodCalls(fn *ssa.Function, visited map[*ssa.Function]bool) {
	if fn == nil || fn.Blocks == nil {
		return
	}
	if visited[fn] {
		return
	}
	visited[fn] = true

	// Detect which blocks are in loops (for loop-based reuse detection)
	loopBlocks := a.detectLoopBlocks(fn)

	// Collect defers for second pass
	var defers []*ssa.Defer

	// First pass: process regular calls and go statements
	for _, block := range fn.Blocks {
		isInLoop := loopBlocks[block]

		for _, instr := range block.Instrs {
			// Check for MakeClosure to process closures recursively
			if mc, ok := instr.(*ssa.MakeClosure); ok {
				if closureFn, ok := mc.Fn.(*ssa.Function); ok {
					// Check if closure captures any *gorm.DB values
					if a.closureCapturesGormDB(mc) {
						a.processMethodCalls(closureFn, visited)
					}
					// Check for bound method (method value) - pollute the receiver
					a.processBoundMethod(mc, closureFn)
				}
				continue
			}

			switch v := instr.(type) {
			case *ssa.Call:
				a.processCall(v, isInLoop, loopBlocks)
			case *ssa.Defer:
				// Collect defers for second pass
				defers = append(defers, v)
			case *ssa.Go:
				// Go statements: process the closure, check for violations
				a.processGoStatement(v)
			case *ssa.Send:
				// Channel send: if sending *gorm.DB, mark as polluted
				a.processSend(v)
			case *ssa.Store:
				// Store to slice/struct field: if storing *gorm.DB, mark as polluted
				a.processStore(v)
			case *ssa.MapUpdate:
				// Map update: if storing *gorm.DB, mark as polluted
				a.processMapUpdate(v)
			case *ssa.MakeInterface:
				// MakeInterface: if wrapping *gorm.DB in interface{}, mark as polluted
				// (may be extracted via type assertion and used elsewhere)
				a.processMakeInterface(v)
			}
		}
	}

	// Second pass: process defer statements (after all regular calls)
	for _, d := range defers {
		a.processDeferStatement(d)
	}
}

// detectLoopBlocks returns a set of blocks that are inside loops.
func (a *usageAnalyzer) detectLoopBlocks(fn *ssa.Function) map[*ssa.BasicBlock]bool {
	loopBlocks := make(map[*ssa.BasicBlock]bool)
	if fn.Blocks == nil {
		return loopBlocks
	}

	// Build block index map
	blockIndex := make(map[*ssa.BasicBlock]int)
	for i, b := range fn.Blocks {
		blockIndex[b] = i
	}

	// Detect back-edges: edge from block B to block A where A dominates B or A appears before B
	// A simpler heuristic: if a block has a successor with a lower index, it's a back-edge
	for _, block := range fn.Blocks {
		for _, succ := range block.Succs {
			if blockIndex[succ] <= blockIndex[block] {
				// Back-edge detected: mark all blocks from succ to block as in-loop
				a.markLoopBlocks(fn, succ, block, loopBlocks, blockIndex)
			}
		}
	}

	return loopBlocks
}

// markLoopBlocks marks blocks that are part of a loop.
func (a *usageAnalyzer) markLoopBlocks(fn *ssa.Function, loopHead, loopTail *ssa.BasicBlock, loopBlocks map[*ssa.BasicBlock]bool, blockIndex map[*ssa.BasicBlock]int) {
	headIdx := blockIndex[loopHead]
	tailIdx := blockIndex[loopTail]

	// Mark all blocks between head and tail (inclusive) as in-loop
	for _, block := range fn.Blocks {
		idx := blockIndex[block]
		if idx >= headIdx && idx <= tailIdx {
			loopBlocks[block] = true
		}
	}
}

// processCall handles a regular *ssa.Call instruction.
func (a *usageAnalyzer) processCall(call *ssa.Call, isInLoop bool, loopBlocks map[*ssa.BasicBlock]bool) {
	// Check if this is a function call that takes *gorm.DB as argument (pollutes it)
	a.checkFunctionCallPollution(call)

	// Check if this is a method call on *gorm.DB
	if !a.isGormDBMethodCall(call) {
		return
	}

	callee := call.Call.StaticCallee()
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

	// Find the mutable root being used
	root := a.findMutableRoot(recv)
	if root == nil {
		return // Receiver is immutable (Session result, parameter, etc.)
	}

	// Get or create state for this root
	state := a.states[root]
	if state == nil {
		state = &valueState{}
		a.states[root] = state
	}

	// Check if root was already polluted
	if state.isPolluted {
		// Using a polluted value - violation!
		state.violations = append(state.violations, call.Pos())
	} else if !isSafeMethod && isTerminal {
		// Terminal Chain Method on unpolluted mutable - pollutes it
		state.isPolluted = true
		state.pollutedAt = call.Pos()

		// If in a loop AND the root is defined outside the loop, report as violation
		// (second iteration will reuse the polluted root)
		if isInLoop && a.isRootDefinedOutsideLoop(root, loopBlocks) {
			state.violations = append(state.violations, call.Pos())
		}
	}
}

// isRootDefinedOutsideLoop checks if the mutable root is defined outside loop blocks.
func (a *usageAnalyzer) isRootDefinedOutsideLoop(root ssa.Value, loopBlocks map[*ssa.BasicBlock]bool) bool {
	// Get the instruction that defines this value
	instr, ok := root.(ssa.Instruction)
	if !ok {
		return true // Non-instruction values (parameters, etc.) are outside loops
	}

	// Check if the defining block is in a loop
	block := instr.Block()
	if block == nil {
		return true
	}

	return !loopBlocks[block]
}

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

		// Get or create state
		state := a.states[root]
		if state == nil {
			state = &valueState{}
			a.states[root] = state
		}

		// Mark as polluted (function call assumed to pollute)
		if !state.isPolluted {
			state.isPolluted = true
			state.pollutedAt = call.Pos()
		}
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

// processDeferStatement handles a defer statement (second pass).
func (a *usageAnalyzer) processDeferStatement(d *ssa.Defer) {
	a.processCallCommon(&d.Call, d.Pos())
}

// processGoStatement handles a go statement.
func (a *usageAnalyzer) processGoStatement(g *ssa.Go) {
	a.processCallCommon(&g.Call, g.Pos())
}

// processCallCommon handles CallCommon from Defer and Go instructions.
// For defer: executed after regular statements, so we check if receiver was polluted.
// For go: executed concurrently, same check applies.
func (a *usageAnalyzer) processCallCommon(callCommon *ssa.CallCommon, pos token.Pos) {
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
		state := a.states[root]
		if state == nil {
			state = &valueState{}
			a.states[root] = state
		}

		// Check if root was already polluted
		if state.isPolluted {
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

		state := a.states[root]
		if state == nil {
			state = &valueState{}
			a.states[root] = state
		}

		// For defer/go, if passing *gorm.DB to a function, check if already polluted
		if state.isPolluted {
			state.violations = append(state.violations, pos)
		}
	}
}

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

	state := a.states[root]
	if state == nil {
		state = &valueState{}
		a.states[root] = state
	}

	// Mark as polluted (channel send assumed to pollute)
	if !state.isPolluted {
		state.isPolluted = true
		state.pollutedAt = send.Pos()
	}
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

	state := a.states[root]
	if state == nil {
		state = &valueState{}
		a.states[root] = state
	}

	// Mark as polluted (slice/struct field store assumed to pollute)
	if !state.isPolluted {
		state.isPolluted = true
		state.pollutedAt = store.Pos()
	}
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

	state := a.states[root]
	if state == nil {
		state = &valueState{}
		a.states[root] = state
	}

	// Mark as polluted (map update assumed to pollute)
	if !state.isPolluted {
		state.isPolluted = true
		state.pollutedAt = mapUpdate.Pos()
	}
}

// processBoundMethod handles bound method (method value) creation.
// If the receiver is *gorm.DB, mark it as polluted since the method
// may be called later and pollute the receiver.
func (a *usageAnalyzer) processBoundMethod(mc *ssa.MakeClosure, fn *ssa.Function) {
	// Check if this is a method (has a receiver)
	sig := fn.Signature
	if sig == nil || sig.Recv() == nil {
		return
	}

	// Check if the receiver type is *gorm.DB
	if !IsGormDB(sig.Recv().Type()) {
		return
	}

	// The first binding is the receiver for bound methods
	if len(mc.Bindings) == 0 {
		return
	}

	recv := mc.Bindings[0]
	root := a.findMutableRoot(recv)
	if root == nil {
		return
	}

	state := a.states[root]
	if state == nil {
		state = &valueState{}
		a.states[root] = state
	}

	// Mark as polluted (bound method may be called and pollute the receiver)
	if !state.isPolluted {
		state.isPolluted = true
		state.pollutedAt = mc.Pos()
	}
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

	state := a.states[root]
	if state == nil {
		state = &valueState{}
		a.states[root] = state
	}

	// Mark as polluted (interface conversion assumed to pollute)
	if !state.isPolluted {
		state.isPolluted = true
		state.pollutedAt = mi.Pos()
	}
}

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

// findMutableRoot finds the mutable root for a receiver value.
// Returns nil if the receiver is immutable (Session result, parameter, etc.)
func (a *usageAnalyzer) findMutableRoot(recv ssa.Value) ssa.Value {
	return a.findMutableRootImpl(recv, make(map[ssa.Value]bool))
}

// findMutableRootImpl recursively finds the mutable root.
func (a *usageAnalyzer) findMutableRootImpl(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	if visited[v] {
		return nil
	}
	visited[v] = true

	// Check if this is an immutable source (Parameter, Safe Method result)
	if a.isImmutableSource(v) {
		return nil
	}

	call, ok := v.(*ssa.Call)
	if !ok {
		// Non-call values - check for Phi, UnOp, etc.
		return a.handleNonCallForRoot(v, visited)
	}

	callee := call.Call.StaticCallee()
	if callee == nil {
		return nil
	}

	sig := callee.Signature
	if sig == nil || sig.Recv() == nil || !IsGormDB(sig.Recv().Type()) {
		// Not a *gorm.DB method - could be a helper function
		// Treat the function return as a mutable root
		if IsGormDB(call.Type()) {
			return call
		}
		return nil
	}

	// This is a *gorm.DB method call
	if len(call.Call.Args) == 0 {
		return nil
	}
	recv := call.Call.Args[0]

	// If receiver is immutable, this call is the mutable root
	if a.isImmutableSource(recv) {
		return call
	}

	// Receiver is mutable - trace back to find the root
	return a.findMutableRootImpl(recv, visited)
}

// handleNonCallForRoot handles non-call values when finding mutable root.
func (a *usageAnalyzer) handleNonCallForRoot(v ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	switch val := v.(type) {
	case *ssa.Phi:
		// For Phi nodes, trace through all edges to find any mutable root.
		// If any edge has a mutable root, return it (conservative for false-negative reduction).
		for _, edge := range val.Edges {
			if root := a.findMutableRootImpl(edge, visited); root != nil {
				return root
			}
		}
		return nil
	case *ssa.UnOp:
		// Dereference (*ptr) - trace through to find the stored value
		if val.Op == token.MUL {
			return a.tracePointerLoad(val.X, visited)
		}
		return a.findMutableRootImpl(val.X, visited)
	case *ssa.ChangeType:
		return a.findMutableRootImpl(val.X, visited)
	case *ssa.Extract:
		return a.findMutableRootImpl(val.Tuple, visited)
	case *ssa.FreeVar:
		// Trace FreeVar back through MakeClosure to find the bound value
		return a.traceFreeVar(val, visited)
	case *ssa.Alloc:
		// Alloc is a heap/stack allocation. Trace to find stored value.
		return a.traceAllocStore(val, visited)
	default:
		return nil
	}
}

// tracePointerLoad traces a pointer load (dereference) to find the mutable root.
// When we have *ptr, we need to find what value was stored to ptr.
func (a *usageAnalyzer) tracePointerLoad(ptr ssa.Value, visited map[ssa.Value]bool) ssa.Value {
	// First, recursively resolve the pointer value (might be FreeVar, Alloc, etc.)
	switch p := ptr.(type) {
	case *ssa.FreeVar:
		// FreeVar pointer - trace back through MakeClosure
		return a.traceFreeVar(p, visited)
	case *ssa.Alloc:
		// Local allocation - find the stored value
		return a.traceAllocStore(p, visited)
	case *ssa.FieldAddr:
		// Struct field - find Store to this field and trace the stored value
		return a.traceFieldStore(p, visited)
	default:
		// Try to trace through other pointer sources
		return a.findMutableRootImpl(ptr, visited)
	}
}

// traceAllocStore finds the value stored to an Alloc instruction.
// Variables captured by closures are allocated on heap and use Store instructions.
func (a *usageAnalyzer) traceAllocStore(alloc *ssa.Alloc, visited map[ssa.Value]bool) ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}

	// Find Store instructions that write to this alloc
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			if store.Addr == alloc {
				// Found a store to this alloc - trace the stored value
				return a.findMutableRootImpl(store.Val, visited)
			}
		}
	}
	return nil
}

// traceFieldStore finds the value stored to a struct field.
// When we have h.field, we find Store instructions that write to the same field.
func (a *usageAnalyzer) traceFieldStore(fa *ssa.FieldAddr, visited map[ssa.Value]bool) ssa.Value {
	fn := fa.Parent()
	if fn == nil {
		return nil
	}

	// Find Store instructions that write to a FieldAddr with same base and field
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			storeFA, ok := store.Addr.(*ssa.FieldAddr)
			if !ok {
				continue
			}
			// Match by same base and same field index
			if storeFA.X == fa.X && storeFA.Field == fa.Field {
				return a.findMutableRootImpl(store.Val, visited)
			}
		}
	}
	return nil
}

// traceFreeVar traces a FreeVar back to the value bound in MakeClosure.
// FreeVars are variables captured from an enclosing function scope.
func (a *usageAnalyzer) traceFreeVar(fv *ssa.FreeVar, visited map[ssa.Value]bool) ssa.Value {
	fn := fv.Parent()
	if fn == nil {
		return nil
	}

	// Find the index of this FreeVar in the function's FreeVars list
	idx := -1
	for i, v := range fn.FreeVars {
		if v == fv {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}

	// Look for MakeClosure instructions in the parent that create this closure
	parent := fn.Parent()
	if parent == nil {
		return nil
	}

	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			mc, ok := instr.(*ssa.MakeClosure)
			if !ok {
				continue
			}
			// Check if this MakeClosure creates our function
			closureFn, ok := mc.Fn.(*ssa.Function)
			if !ok || closureFn != fn {
				continue
			}
			// mc.Bindings[idx] is the value bound to this FreeVar
			if idx < len(mc.Bindings) {
				return a.findMutableRootImpl(mc.Bindings[idx], visited)
			}
		}
	}
	return nil
}

// collectViolations collects all detected violations.
func (a *usageAnalyzer) collectViolations() []violation {
	var violations []violation

	for _, state := range a.states {
		for _, pos := range state.violations {
			violations = append(violations, violation{
				pos:     pos,
				message: "*gorm.DB instance reused after chain method (use .Session(&gorm.Session{}) to make it safe)",
			})
		}
	}

	return violations
}

// isImmutableSource checks if a value is an immutable source.
// This includes: Session/WithContext results, function parameters, and DB init methods.
// Note: FreeVar is NOT immutable - it needs to be traced back through MakeClosure.
func (a *usageAnalyzer) isImmutableSource(v ssa.Value) bool {
	switch val := v.(type) {
	case *ssa.Parameter:
		return true
	case *ssa.Call:
		callee := val.Call.StaticCallee()
		if callee == nil {
			return false
		}
		// Safe Methods return immutable
		if IsSafeMethod(callee.Name()) {
			return true
		}
		// DB Init Methods return immutable
		if IsDBInitMethod(callee.Name()) {
			return true
		}
		// Function calls returning *gorm.DB are treated as mutable
		// (we don't know what they do internally)
		return false
	default:
		return false
	}
}
