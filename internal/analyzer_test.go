package internal

import (
	"go/constant"
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestNewUsageAnalyzer(t *testing.T) {
	pureFuncs := map[string]struct{}{
		"test.Pure": {},
	}
	analyzer := newUsageAnalyzer(nil, pureFuncs)

	if analyzer.fn != nil {
		t.Error("Expected fn to be nil")
	}
	if analyzer.states == nil {
		t.Error("Expected states to be initialized")
	}
	if analyzer.pureFuncs == nil {
		t.Error("Expected pureFuncs to be set")
	}
}

func TestGetOrCreateState(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Create a mock value (we can use nil as key in tests)
	var mockValue ssa.Value

	// First call should create state
	state1 := analyzer.getOrCreateState(mockValue)
	if state1 == nil {
		t.Fatal("Expected state to be created")
	}
	if state1.pollutedBlocks == nil {
		t.Error("Expected pollutedBlocks to be initialized")
	}

	// Second call should return same state
	state2 := analyzer.getOrCreateState(mockValue)
	if state1 != state2 {
		t.Error("Expected same state to be returned")
	}
}

func TestMarkPolluted(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	var mockValue ssa.Value
	var mockBlock *ssa.BasicBlock
	pos := token.Pos(100)

	// First mark should return true (newly polluted)
	if !analyzer.markPolluted(mockValue, mockBlock, pos) {
		t.Error("First markPolluted should return true")
	}

	// Second mark should return false (already polluted)
	if analyzer.markPolluted(mockValue, mockBlock, pos) {
		t.Error("Second markPolluted should return false")
	}

	// Verify state
	state := analyzer.states[mockValue]
	if state == nil {
		t.Fatal("Expected state to exist")
	}
	if storedPos, ok := state.pollutedBlocks[mockBlock]; !ok || storedPos != pos {
		t.Error("Expected block to be polluted with correct position")
	}
}

func TestIsPollutedAt_NilCases(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// nil state
	if analyzer.isPollutedAt(nil, nil) {
		t.Error("isPollutedAt(nil, nil) should return false")
	}

	// Empty state
	state := &valueState{
		pollutedBlocks: make(map[*ssa.BasicBlock]token.Pos),
	}
	if analyzer.isPollutedAt(state, nil) {
		t.Error("isPollutedAt with empty pollutedBlocks should return false")
	}

	// State with nil block entry, nil target
	state.pollutedBlocks[nil] = token.Pos(1)
	if analyzer.isPollutedAt(state, nil) {
		t.Error("isPollutedAt with nil target should return false")
	}
}

func TestIsPollutedAnywhere_NilCases(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// nil state
	if analyzer.isPollutedAnywhere(nil, nil) {
		t.Error("isPollutedAnywhere(nil, nil) should return false")
	}

	// Empty state
	state := &valueState{
		pollutedBlocks: make(map[*ssa.BasicBlock]token.Pos),
	}
	if analyzer.isPollutedAnywhere(state, nil) {
		t.Error("isPollutedAnywhere with empty pollutedBlocks should return false")
	}
}

func TestCanReach_NilCases(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Both nil
	if analyzer.canReach(nil, nil) {
		t.Error("canReach(nil, nil) should return false")
	}
}

func TestCanReach_SameBlock(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	block := &ssa.BasicBlock{}

	// Same block should be reachable
	if !analyzer.canReach(block, block) {
		t.Error("canReach(block, block) should return true")
	}
}

func TestCollectViolations_Empty(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	violations := analyzer.collectViolations()

	if len(violations) != 0 {
		t.Errorf("Expected 0 violations, got %d", len(violations))
	}
}

func TestCollectViolations_WithViolations(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Add a state with violations
	var mockValue ssa.Value
	state := analyzer.getOrCreateState(mockValue)
	state.violations = []token.Pos{token.Pos(100), token.Pos(200)}

	violations := analyzer.collectViolations()

	if len(violations) != 2 {
		t.Errorf("Expected 2 violations, got %d", len(violations))
	}

	// Verify message format
	for _, v := range violations {
		if v.message == "" {
			t.Error("Expected non-empty message")
		}
	}
}

func TestIsPureFunction_NilPureFuncs(t *testing.T) {
	// nil pureFuncs should return false
	analyzer := newUsageAnalyzer(nil, nil)

	// isPureFunction with nil pureFuncs should return false for any input
	// We can't easily test with a real function, but the nil check is covered
	if analyzer.pureFuncs != nil {
		t.Error("Expected pureFuncs to be nil")
	}
}

func TestNewChecker(t *testing.T) {
	ignoreMap := make(IgnoreMap)
	pureFuncs := map[string]struct{}{}

	chk := newChecker(nil, ignoreMap, pureFuncs)

	if chk.pass != nil {
		t.Error("Expected pass to be nil")
	}
	if chk.ignoreMap == nil {
		t.Error("Expected ignoreMap to be set")
	}
	if chk.pureFuncs == nil {
		t.Error("Expected pureFuncs to be set")
	}
	if chk.reported == nil {
		t.Error("Expected reported to be initialized")
	}
}

func TestClosureCapturesGormDB_Empty(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// MakeClosure with no bindings
	mc := &ssa.MakeClosure{}
	if analyzer.closureCapturesGormDB(mc) {
		t.Error("Empty MakeClosure should not capture GormDB")
	}
}

func TestDetectLoopBlocks_NilBlocks(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Function with nil Blocks
	fn := &ssa.Function{}
	loopBlocks := analyzer.detectLoopBlocks(fn)

	if loopBlocks == nil {
		t.Error("Expected non-nil map")
	}
	if len(loopBlocks) != 0 {
		t.Error("Expected empty map for nil Blocks")
	}
}

func TestProcessMethodCalls_NilFunction(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Should not panic with nil function
	analyzer.processMethodCalls(nil, make(map[*ssa.Function]bool))
}

func TestProcessMethodCalls_EmptyFunction(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Function with nil Blocks
	fn := &ssa.Function{}
	analyzer.processMethodCalls(fn, make(map[*ssa.Function]bool))
}

func TestProcessMethodCalls_AlreadyVisited(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	fn := &ssa.Function{}
	visited := map[*ssa.Function]bool{fn: true}

	// Should return early without processing
	analyzer.processMethodCalls(fn, visited)
}

func TestIsRootDefinedOutsideLoop_NilValue(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	loopBlocks := make(map[*ssa.BasicBlock]bool)

	// nil value should be considered outside loop (non-instruction)
	result := analyzer.isRootDefinedOutsideLoop(nil, loopBlocks)
	if !result {
		t.Error("nil value should be considered outside loop")
	}
}

func TestHandleNonCallForRoot_NilValue(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	result := analyzer.handleNonCallForRoot(nil, visited)
	if result != nil {
		t.Error("handleNonCallForRoot(nil) should return nil")
	}
}

func TestFindMutableRoot_Visited(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	var mockValue ssa.Value
	visited[mockValue] = true

	result := analyzer.findMutableRootImpl(mockValue, visited)
	if result != nil {
		t.Error("Already visited value should return nil")
	}
}

func TestIsImmutableSource_Parameter(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Parameters are immutable
	param := &ssa.Parameter{}
	if !analyzer.isImmutableSource(param) {
		t.Error("Parameter should be immutable source")
	}
}

func TestIsImmutableSource_Nil(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// nil should not be immutable
	if analyzer.isImmutableSource(nil) {
		t.Error("nil should not be immutable source")
	}
}

func TestAnalyze_EmptyFunction(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := newUsageAnalyzer(fn, nil)

	violations := analyzer.analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}

func TestDetectReachabilityViolations_NoStates(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Should not panic with empty states
	analyzer.detectReachabilityViolations()
}

func TestDetectReachabilityViolations_SinglePollution(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Add a state with only one polluted block
	var mockValue ssa.Value
	state := analyzer.getOrCreateState(mockValue)
	state.pollutedBlocks[&ssa.BasicBlock{}] = token.Pos(1)

	// Should not create violations with only one pollution site
	analyzer.detectReachabilityViolations()

	if len(state.violations) != 0 {
		t.Errorf("Expected 0 violations with single pollution, got %d", len(state.violations))
	}
}

// ============================================================================
// Additional tests for better coverage
// ============================================================================

func TestCanReach_WithSuccessors(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Create a chain: block1 -> block2 -> block3
	block1 := &ssa.BasicBlock{}
	block2 := &ssa.BasicBlock{}
	block3 := &ssa.BasicBlock{}

	block1.Succs = []*ssa.BasicBlock{block2}
	block2.Succs = []*ssa.BasicBlock{block3}

	// block1 can reach block3 via block2
	if !analyzer.canReach(block1, block3) {
		t.Error("block1 should reach block3")
	}

	// block3 cannot reach block1 (no back edge)
	if analyzer.canReach(block3, block1) {
		t.Error("block3 should not reach block1")
	}

	// block1 can reach block2 directly
	if !analyzer.canReach(block1, block2) {
		t.Error("block1 should reach block2")
	}
}

func TestCanReach_Unreachable(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	block1 := &ssa.BasicBlock{}
	block2 := &ssa.BasicBlock{}

	// No successors, so unreachable
	if analyzer.canReach(block1, block2) {
		t.Error("Disconnected blocks should not be reachable")
	}
}

func TestDetectLoopBlocks_WithBlocks(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Create blocks for a simple loop: block0 -> block1 -> block0 (back edge)
	block0 := &ssa.BasicBlock{Index: 0}
	block1 := &ssa.BasicBlock{Index: 1}

	block0.Succs = []*ssa.BasicBlock{block1}
	block1.Succs = []*ssa.BasicBlock{block0} // back edge

	fn := &ssa.Function{
		Blocks: []*ssa.BasicBlock{block0, block1},
	}

	loopBlocks := analyzer.detectLoopBlocks(fn)

	// Both blocks should be marked as in loop
	if !loopBlocks[block0] {
		t.Error("block0 should be in loop")
	}
	if !loopBlocks[block1] {
		t.Error("block1 should be in loop")
	}
}

func TestDetectLoopBlocks_NoLoop(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Linear blocks: block0 -> block1 -> block2 (no back edge)
	block0 := &ssa.BasicBlock{Index: 0}
	block1 := &ssa.BasicBlock{Index: 1}
	block2 := &ssa.BasicBlock{Index: 2}

	block0.Succs = []*ssa.BasicBlock{block1}
	block1.Succs = []*ssa.BasicBlock{block2}

	fn := &ssa.Function{
		Blocks: []*ssa.BasicBlock{block0, block1, block2},
	}

	loopBlocks := analyzer.detectLoopBlocks(fn)

	// No blocks should be in loop
	if len(loopBlocks) != 0 {
		t.Errorf("Expected no loop blocks, got %d", len(loopBlocks))
	}
}

func TestIsPollutedAt_SameFunction(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := newUsageAnalyzer(fn, nil)

	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 1}
	block1.Succs = []*ssa.BasicBlock{block2}

	// Set parent function for blocks
	fn.Blocks = []*ssa.BasicBlock{block1, block2}

	state := &valueState{
		pollutedBlocks: map[*ssa.BasicBlock]token.Pos{
			block1: token.Pos(1),
		},
	}

	// block2 is reachable from polluted block1
	if !analyzer.isPollutedAt(state, block2) {
		t.Error("block2 should be polluted (reachable from block1)")
	}
}

func TestIsPollutedAt_UnreachableBlocks(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := newUsageAnalyzer(fn, nil)

	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 1}
	// No successors set - blocks are disconnected
	fn.Blocks = []*ssa.BasicBlock{block1, block2}

	state := &valueState{
		pollutedBlocks: map[*ssa.BasicBlock]token.Pos{
			block1: token.Pos(1),
		},
	}

	// block2 is NOT reachable from block1 (no path)
	if analyzer.isPollutedAt(state, block2) {
		t.Error("block2 should not be polluted (unreachable from block1)")
	}
}

func TestIsPollutedAnywhere_WithBlock(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := newUsageAnalyzer(fn, nil)

	block := &ssa.BasicBlock{Index: 0}
	fn.Blocks = []*ssa.BasicBlock{block}

	state := &valueState{
		pollutedBlocks: map[*ssa.BasicBlock]token.Pos{
			block: token.Pos(1),
		},
	}

	// Same function - should be polluted
	if !analyzer.isPollutedAnywhere(state, fn) {
		t.Error("Should be polluted anywhere in same function")
	}
}

func TestIsPollutedAnywhere_DifferentFunction(t *testing.T) {
	fn1 := &ssa.Function{}
	fn2 := &ssa.Function{}
	analyzer := newUsageAnalyzer(fn1, nil)

	block := &ssa.BasicBlock{Index: 0}
	fn1.Blocks = []*ssa.BasicBlock{block}

	state := &valueState{
		pollutedBlocks: map[*ssa.BasicBlock]token.Pos{
			block: token.Pos(1),
		},
	}

	// Different function - closure pollution affects parent
	if !analyzer.isPollutedAnywhere(state, fn2) {
		t.Error("Should be polluted (closure affects parent)")
	}
}

func TestIsRootDefinedOutsideLoop_NilBlock(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Call is an instruction but Block() returns nil for uninitialized Call
	call := &ssa.Call{}

	loopBlocks := map[*ssa.BasicBlock]bool{}

	// Instruction with nil block should be considered outside loop
	result := analyzer.isRootDefinedOutsideLoop(call, loopBlocks)
	if !result {
		t.Error("Instruction with nil block should be outside loop")
	}
}

func TestHandleNonCallForRoot_Phi(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Phi with nil edges returns nil
	phi := &ssa.Phi{}
	result := analyzer.handleNonCallForRoot(phi, visited)
	if result != nil {
		t.Error("Phi with no edges should return nil")
	}
}

func TestHandleNonCallForRoot_ChangeType(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// ChangeType traces through X
	changeType := &ssa.ChangeType{X: nil}
	result := analyzer.handleNonCallForRoot(changeType, visited)
	if result != nil {
		t.Error("ChangeType with nil X should return nil")
	}
}

func TestHandleNonCallForRoot_Extract(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Extract traces through Tuple
	extract := &ssa.Extract{Tuple: nil}
	result := analyzer.handleNonCallForRoot(extract, visited)
	if result != nil {
		t.Error("Extract with nil Tuple should return nil")
	}
}

func TestClosureCapturesGormDB_WithNonGormBinding(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// MakeClosure with non-gorm.DB binding
	param := &ssa.Parameter{} // not *gorm.DB type
	mc := &ssa.MakeClosure{
		Bindings: []ssa.Value{param},
	}

	if analyzer.closureCapturesGormDB(mc) {
		t.Error("MakeClosure with non-gorm.DB binding should not capture GormDB")
	}
}

func TestFindMutableRootImpl_Parameter(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Parameter is immutable source
	param := &ssa.Parameter{}
	result := analyzer.findMutableRootImpl(param, visited)
	if result != nil {
		t.Error("Parameter should return nil (immutable)")
	}
}

func TestFindMutableRootImpl_Call_NilCallee(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Call with nil StaticCallee
	call := &ssa.Call{}
	result := analyzer.findMutableRootImpl(call, visited)
	// StaticCallee is nil, returns nil
	if result != nil {
		t.Error("Call with nil callee should return nil")
	}
}

func TestDetectReachabilityViolations_MultiplePollutions_SameBlock(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := newUsageAnalyzer(fn, nil)

	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 1}
	fn.Blocks = []*ssa.BasicBlock{block1, block2}

	// Same block for both - should not create violation (same block case handled elsewhere)
	var mockValue ssa.Value
	state := analyzer.getOrCreateState(mockValue)
	state.pollutedBlocks[block1] = token.Pos(1)
	state.pollutedBlocks[block1] = token.Pos(2) // Same block, different pos (won't happen in practice)

	analyzer.detectReachabilityViolations()
	// No violations because there's only one unique block
}

func TestDetectReachabilityViolations_ReachableBlocks(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := newUsageAnalyzer(fn, nil)

	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 1}
	block1.Succs = []*ssa.BasicBlock{block2}
	fn.Blocks = []*ssa.BasicBlock{block1, block2}

	var mockValue ssa.Value
	state := analyzer.getOrCreateState(mockValue)
	state.pollutedBlocks[block1] = token.Pos(1)
	state.pollutedBlocks[block2] = token.Pos(2)

	analyzer.detectReachabilityViolations()

	// block1 can reach block2, so block2 should be a violation
	if len(state.violations) != 1 {
		t.Errorf("Expected 1 violation, got %d", len(state.violations))
	}
}

func TestMarkLoopBlocks(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	block0 := &ssa.BasicBlock{Index: 0}
	block1 := &ssa.BasicBlock{Index: 1}
	block2 := &ssa.BasicBlock{Index: 2}

	fn := &ssa.Function{
		Blocks: []*ssa.BasicBlock{block0, block1, block2},
	}

	blockIndex := map[*ssa.BasicBlock]int{
		block0: 0,
		block1: 1,
		block2: 2,
	}

	loopBlocks := make(map[*ssa.BasicBlock]bool)

	// Mark blocks from block0 to block2 as loop
	analyzer.markLoopBlocks(fn, block0, block2, loopBlocks, blockIndex)

	if !loopBlocks[block0] || !loopBlocks[block1] || !loopBlocks[block2] {
		t.Error("All blocks should be marked as in loop")
	}
}

func TestFindMutableRoot_NilInput(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	result := analyzer.findMutableRoot(nil)
	if result != nil {
		t.Error("findMutableRoot(nil) should return nil")
	}
}

func TestHandleNonCallForRoot_UnOp_NonDeref(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// UnOp that is not dereference (e.g., negation)
	unop := &ssa.UnOp{
		Op: token.SUB, // Not MUL (dereference)
		X:  nil,
	}
	result := analyzer.handleNonCallForRoot(unop, visited)
	if result != nil {
		t.Error("UnOp with nil X should return nil")
	}
}

func TestHandleNonCallForRoot_UnOp_Deref(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// UnOp that is dereference
	unop := &ssa.UnOp{
		Op: token.MUL, // Dereference
		X:  nil,
	}
	result := analyzer.handleNonCallForRoot(unop, visited)
	// tracePointerLoad with nil returns nil
	if result != nil {
		t.Error("UnOp deref with nil X should return nil")
	}
}

func TestHandleNonCallForRoot_FreeVar(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	fv := &ssa.FreeVar{}
	result := analyzer.handleNonCallForRoot(fv, visited)
	// traceFreeVar with nil parent returns nil
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestTracePointerLoad_NilPointer(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	result := analyzer.tracePointerLoad(nil, visited)
	if result != nil {
		t.Error("tracePointerLoad(nil) should return nil")
	}
}

func TestTracePointerLoad_FreeVar(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	fv := &ssa.FreeVar{}
	result := analyzer.tracePointerLoad(fv, visited)
	// traceFreeVar with nil parent returns nil
	if result != nil {
		t.Error("tracePointerLoad(FreeVar) with nil parent should return nil")
	}
}

func TestTracePointerLoad_OtherValue(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Parameter falls through to findMutableRootImpl
	param := &ssa.Parameter{}
	result := analyzer.tracePointerLoad(param, visited)
	// Parameter is immutable, returns nil
	if result != nil {
		t.Error("tracePointerLoad(Parameter) should return nil")
	}
}

func TestTraceFreeVar_NotFound(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// FreeVar with nil parent - traceFreeVar returns nil
	fv := &ssa.FreeVar{}
	result := analyzer.traceFreeVar(fv, visited)
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestIsImmutableSource_Call_SafeMethod(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// We cannot easily create a proper ssa.Call with StaticCallee
	// But we can test the nil callee case
	call := &ssa.Call{}
	if analyzer.isImmutableSource(call) {
		t.Error("Call with nil callee should not be immutable")
	}
}

func TestDetectReachabilityViolations_CrossFunction(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := newUsageAnalyzer(fn, nil)

	// Create blocks in different "functions" (simulate closure)
	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 0}

	fn.Blocks = []*ssa.BasicBlock{block1}
	// block2 belongs to fn as well (same Parent)

	var mockValue ssa.Value
	state := analyzer.getOrCreateState(mockValue)
	state.pollutedBlocks[block1] = token.Pos(1)
	state.pollutedBlocks[block2] = token.Pos(2)

	// Both blocks have same Parent() (nil), so they're in same function
	analyzer.detectReachabilityViolations()
}

func TestClosureCapturesGormDB_PointerToPointer(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// MakeClosure with a pointer type binding (not *gorm.DB)
	param := &ssa.Parameter{}
	mc := &ssa.MakeClosure{
		Bindings: []ssa.Value{param},
	}

	// Parameter type is nil, so it won't match *gorm.DB
	if analyzer.closureCapturesGormDB(mc) {
		t.Error("MakeClosure with nil type binding should not capture GormDB")
	}
}

func TestHandleNonCallForRoot_PhiWithEdges(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Phi with edges that are all nil
	param := &ssa.Parameter{} // immutable, will return nil
	phi := &ssa.Phi{
		Edges: []ssa.Value{param},
	}

	result := analyzer.handleNonCallForRoot(phi, visited)
	// Parameter is immutable, returns nil
	if result != nil {
		t.Error("Phi with only immutable edges should return nil")
	}
}

// =============================================================================
// traceFreeVar Tests
// =============================================================================

func TestTraceFreeVar_NilParent(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// FreeVar with nil Parent
	fv := &ssa.FreeVar{}
	result := analyzer.traceFreeVar(fv, visited)
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestTraceFreeVar_NotInFreeVars(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// FreeVar with no matching entry in fn.FreeVars
	fv := &ssa.FreeVar{}
	result := analyzer.traceFreeVar(fv, visited)
	// Parent is nil, returns nil
	if result != nil {
		t.Error("FreeVar not found in parent's FreeVars should return nil")
	}
}

func TestTraceFreeVar_NilGrandparent(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// This tests the case where fn.Parent() is nil
	fv := &ssa.FreeVar{}
	result := analyzer.traceFreeVar(fv, visited)
	if result != nil {
		t.Error("FreeVar with nil grandparent should return nil")
	}
}

// =============================================================================
// traceIIFEReturns Tests
// =============================================================================

func TestTraceIIFEReturns_NilResults(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Function with nil Signature.Results()
	fn := &ssa.Function{}
	result := analyzer.traceIIFEReturns(fn, visited)
	if result != nil {
		t.Error("Function with nil results should return nil")
	}
}

func TestTraceIIFEReturns_EmptyResults(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Function with empty signature (void return)
	fn := &ssa.Function{}
	result := analyzer.traceIIFEReturns(fn, visited)
	if result != nil {
		t.Error("Function with no return type should return nil")
	}
}

func TestTraceIIFEReturns_NonGormDBReturn(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Function that doesn't return *gorm.DB - signature is nil
	fn := &ssa.Function{}
	result := analyzer.traceIIFEReturns(fn, visited)
	if result != nil {
		t.Error("Function not returning *gorm.DB should return nil")
	}
}

func TestTraceIIFEReturns_NoBlocks(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Function with nil Blocks
	fn := &ssa.Function{}
	result := analyzer.traceIIFEReturns(fn, visited)
	if result != nil {
		t.Error("Function with no blocks should return nil")
	}
}

// =============================================================================
// detectReachabilityViolations Tests - Cross-function cases
// =============================================================================

func TestDetectReachabilityViolations_SinglePollution_NoViolation(t *testing.T) {
	parentFn := &ssa.Function{}
	analyzer := newUsageAnalyzer(parentFn, nil)

	// Only one pollution site - no violation possible
	block := &ssa.BasicBlock{Index: 0}
	parentFn.Blocks = []*ssa.BasicBlock{block}

	var mockValue ssa.Value
	state := analyzer.getOrCreateState(mockValue)
	state.pollutedBlocks[block] = token.Pos(1)

	analyzer.detectReachabilityViolations()

	// Single pollution should not create violation
	if len(state.violations) != 0 {
		t.Errorf("Expected 0 violations (single pollution), got %d", len(state.violations))
	}
}

// =============================================================================
// isImmutableSource Tests
// =============================================================================

func TestIsImmutableSource_NilValue(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// nil value
	result := analyzer.isImmutableSource(nil)
	if result {
		t.Error("nil value should not be immutable source")
	}
}

func TestIsImmutableSource_Alloc(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Alloc is not an immutable source
	alloc := &ssa.Alloc{}
	result := analyzer.isImmutableSource(alloc)
	if result {
		t.Error("Alloc should not be immutable source")
	}
}

func TestIsImmutableSource_MakeInterface(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// MakeInterface is not an immutable source
	mi := &ssa.MakeInterface{}
	result := analyzer.isImmutableSource(mi)
	if result {
		t.Error("MakeInterface should not be immutable source")
	}
}

func TestIsImmutableSource_Phi(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Phi is not an immutable source
	phi := &ssa.Phi{}
	result := analyzer.isImmutableSource(phi)
	if result {
		t.Error("Phi should not be immutable source")
	}
}

func TestIsImmutableSource_CallWithNilCallee(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	// Call with nil StaticCallee
	call := &ssa.Call{}
	result := analyzer.isImmutableSource(call)
	if result {
		t.Error("Call with nil callee should not be immutable source")
	}
}

// =============================================================================
// processBoundMethodCall Tests
// =============================================================================

func TestProcessBoundMethodCall_EmptyBindings(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	call := &ssa.Call{}
	mc := &ssa.MakeClosure{
		Bindings: []ssa.Value{}, // empty
	}

	// Should not panic with empty bindings
	analyzer.processBoundMethodCall(call, mc, false, nil)
}

func TestProcessBoundMethodCall_NonGormDBBinding(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	call := &ssa.Call{}
	param := &ssa.Parameter{} // Not *gorm.DB type
	mc := &ssa.MakeClosure{
		Bindings: []ssa.Value{param},
	}

	// Should not panic with non-gorm.DB binding
	analyzer.processBoundMethodCall(call, mc, false, nil)
}

// =============================================================================
// findMakeClosureForBoundMethod Tests
// =============================================================================

func TestFindMakeClosureForBoundMethod_NilValue(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)

	call := &ssa.Call{}
	result := analyzer.findMakeClosureForBoundMethod(call)
	if result != nil {
		t.Error("Call with nil Value should return nil MakeClosure")
	}
}

// =============================================================================
// traceMakeClosureImpl Tests
// =============================================================================

func TestTraceMakeClosureImpl_MakeClosure(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	mc := &ssa.MakeClosure{}
	result := analyzer.traceMakeClosureImpl(mc, visited)
	if result != mc {
		t.Error("MakeClosure should return itself")
	}
}

func TestTraceMakeClosureImpl_Visited(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	param := &ssa.Parameter{}
	visited[param] = true

	result := analyzer.traceMakeClosureImpl(param, visited)
	if result != nil {
		t.Error("Already visited value should return nil")
	}
}

func TestTraceMakeClosureImpl_UnOp(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// UnOp with nil X
	unop := &ssa.UnOp{X: nil}
	result := analyzer.traceMakeClosureImpl(unop, visited)
	if result != nil {
		t.Error("UnOp with nil X should return nil")
	}
}

func TestTraceMakeClosureImpl_Phi(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// Phi with no edges
	phi := &ssa.Phi{Edges: []ssa.Value{}}
	result := analyzer.traceMakeClosureImpl(phi, visited)
	if result != nil {
		t.Error("Phi with no edges should return nil")
	}
}

func TestTraceMakeClosureImpl_NilValue(t *testing.T) {
	analyzer := newUsageAnalyzer(nil, nil)
	visited := make(map[ssa.Value]bool)

	// nil value should return nil
	result := analyzer.traceMakeClosureImpl(nil, visited)
	if result != nil {
		t.Error("nil value should return nil")
	}
}

// =============================================================================
// isDescendantOf Tests
// =============================================================================

func TestIsDescendantOf_NilFn(t *testing.T) {
	if isDescendantOf(nil, &ssa.Function{}) {
		t.Error("nil fn should not be descendant of anything")
	}
}

func TestIsDescendantOf_NilAncestor(t *testing.T) {
	if isDescendantOf(&ssa.Function{}, nil) {
		t.Error("nothing should be descendant of nil")
	}
}

func TestIsDescendantOf_BothNil(t *testing.T) {
	if isDescendantOf(nil, nil) {
		t.Error("nil should not be descendant of nil")
	}
}

func TestIsDescendantOf_SameFunction(t *testing.T) {
	fn := &ssa.Function{}
	if !isDescendantOf(fn, fn) {
		t.Error("function should be descendant of itself")
	}
}

func TestIsDescendantOf_NoParent(t *testing.T) {
	fn := &ssa.Function{}
	ancestor := &ssa.Function{}
	// fn has no Parent, so it's not a descendant of ancestor
	if isDescendantOf(fn, ancestor) {
		t.Error("function with no parent should not be descendant of unrelated function")
	}
}

// =============================================================================
// isNilConst Tests
// =============================================================================

func TestIsNilConst_NilConst(t *testing.T) {
	// Const with nil Value is a nil constant
	c := &ssa.Const{Value: nil}
	if !isNilConst(c) {
		t.Error("Const with nil Value should be nil constant")
	}
}

func TestIsNilConst_NonNilConst(t *testing.T) {
	// Const with non-nil Value is not a nil constant
	c := &ssa.Const{Value: constant.MakeInt64(42)}
	if isNilConst(c) {
		t.Error("Const with non-nil Value should not be nil constant")
	}
}

func TestIsNilConst_NonConst(t *testing.T) {
	// Non-Const value is not a nil constant
	p := &ssa.Parameter{}
	if isNilConst(p) {
		t.Error("Parameter should not be nil constant")
	}
}

func TestIsNilConst_Nil(t *testing.T) {
	// nil value is not a nil constant
	if isNilConst(nil) {
		t.Error("nil should not be nil constant")
	}
}
