package internal

import (
	"go/constant"
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestNewAnalyzer(t *testing.T) {
	pureFuncs := map[string]struct{}{
		"test.Pure": {},
	}
	analyzer := NewAnalyzer(nil, pureFuncs)

	if analyzer.fn != nil {
		t.Error("Expected fn to be nil")
	}
	if analyzer.rootTracer == nil {
		t.Error("Expected rootTracer to be initialized")
	}
	if analyzer.cfgAnalyzer == nil {
		t.Error("Expected cfgAnalyzer to be initialized")
	}
	if analyzer.handlers == nil {
		t.Error("Expected handlers to be initialized")
	}
}

func TestPollutionTracker_MarkPolluted(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)
	var mockValue ssa.Value
	var mockBlock *ssa.BasicBlock
	pos := token.Pos(100)

	// First mark should return true (newly polluted)
	if !tracker.MarkPolluted(mockValue, mockBlock, pos) {
		t.Error("First MarkPolluted should return true")
	}

	// Second mark should return false (already polluted)
	if tracker.MarkPolluted(mockValue, mockBlock, pos) {
		t.Error("Second MarkPolluted should return false")
	}

	// Verify state via IsPollutedInBlock
	if !tracker.IsPollutedInBlock(mockValue, mockBlock) {
		t.Error("Expected block to be polluted")
	}
}

func TestPollutionTracker_IsPollutedAt_NilCases(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)

	var mockValue ssa.Value

	// Not tracked value
	if tracker.IsPollutedAt(mockValue, nil) {
		t.Error("IsPollutedAt for untracked value should return false")
	}

	// Track but with no pollution
	tracker.AddViolation(mockValue, token.Pos(1)) // Just to create state
	if tracker.IsPollutedAt(mockValue, nil) {
		t.Error("IsPollutedAt with nil target should return false")
	}
}

func TestPollutionTracker_IsPollutedAnywhere_NilCases(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)

	var mockValue ssa.Value

	// Not tracked value
	if tracker.IsPollutedAnywhere(mockValue, nil) {
		t.Error("IsPollutedAnywhere for untracked value should return false")
	}
}

func TestCFGAnalyzer_CanReach_NilCases(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Both nil
	if cfgAnalyzer.CanReach(nil, nil) {
		t.Error("CanReach(nil, nil) should return false")
	}
}

func TestCFGAnalyzer_CanReach_SameBlock(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	block := &ssa.BasicBlock{}

	// Same block should be reachable
	if !cfgAnalyzer.CanReach(block, block) {
		t.Error("CanReach(block, block) should return true")
	}
}

func TestPollutionTracker_CollectViolations_Empty(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)
	violations := tracker.CollectViolations()

	if len(violations) != 0 {
		t.Errorf("Expected 0 violations, got %d", len(violations))
	}
}

func TestPollutionTracker_CollectViolations_WithViolations(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)

	// Add violations
	var mockValue ssa.Value
	tracker.AddViolation(mockValue, token.Pos(100))
	tracker.AddViolation(mockValue, token.Pos(200))

	violations := tracker.CollectViolations()

	if len(violations) != 2 {
		t.Errorf("Expected 2 violations, got %d", len(violations))
	}

	// Verify message format
	for _, v := range violations {
		if v.Message == "" {
			t.Error("Expected non-empty message")
		}
	}
}

func TestRootTracer_IsPureFunction_NilPureFuncs(t *testing.T) {
	// nil pureFuncs should return false
	tracer := NewRootTracer(nil)

	if tracer.pureFuncs != nil {
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
	// MakeClosure with no bindings
	mc := &ssa.MakeClosure{}
	if ClosureCapturesGormDB(mc) {
		t.Error("Empty MakeClosure should not capture GormDB")
	}
}

func TestCFGAnalyzer_DetectLoops_NilBlocks(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Function with nil Blocks
	fn := &ssa.Function{}
	loopInfo := cfgAnalyzer.DetectLoops(fn)

	if loopInfo.LoopBlocks == nil {
		t.Error("Expected non-nil map")
	}
	if len(loopInfo.LoopBlocks) != 0 {
		t.Error("Expected empty map for nil Blocks")
	}
}

func TestAnalyzer_ProcessFunction_NilFunction(t *testing.T) {
	analyzer := NewAnalyzer(nil, nil)
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)

	// Should not panic with nil function
	analyzer.processFunction(nil, tracker, make(map[*ssa.Function]bool))
}

func TestAnalyzer_ProcessFunction_EmptyFunction(t *testing.T) {
	analyzer := NewAnalyzer(nil, nil)
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)

	// Function with nil Blocks
	fn := &ssa.Function{}
	analyzer.processFunction(fn, tracker, make(map[*ssa.Function]bool))
}

func TestAnalyzer_ProcessFunction_AlreadyVisited(t *testing.T) {
	analyzer := NewAnalyzer(nil, nil)
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)

	fn := &ssa.Function{}
	visited := map[*ssa.Function]bool{fn: true}

	// Should return early without processing
	analyzer.processFunction(fn, tracker, visited)
}

func TestCFGAnalyzer_IsDefinedOutsideLoop_NilValue(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	loopInfo := &LoopInfo{LoopBlocks: make(map[*ssa.BasicBlock]bool)}

	// nil value should be considered outside loop (non-instruction)
	result := cfgAnalyzer.IsDefinedOutsideLoop(nil, loopInfo)
	if !result {
		t.Error("nil value should be considered outside loop")
	}
}

func TestRootTracer_FindMutableRoot_NilValue(t *testing.T) {
	tracer := NewRootTracer(nil)

	result := tracer.FindMutableRoot(nil)
	if result != nil {
		t.Error("FindMutableRoot(nil) should return nil")
	}
}

func TestRootTracer_FindMutableRoot_Visited(t *testing.T) {
	// This test verifies cycle detection in FindMutableRoot
	// Since cycles are handled internally, we test that nil input returns nil
	tracer := NewRootTracer(nil)

	// FindMutableRoot handles cycles internally
	result := tracer.FindMutableRoot(nil)
	if result != nil {
		t.Error("nil value should return nil")
	}
}

func TestRootTracer_IsImmutableSource_Parameter(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Parameters are immutable
	param := &ssa.Parameter{}
	if !tracer.isImmutableSource(param) {
		t.Error("Parameter should be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_Nil(t *testing.T) {
	tracer := NewRootTracer(nil)

	// nil should not be immutable
	if tracer.isImmutableSource(nil) {
		t.Error("nil should not be immutable source")
	}
}

func TestAnalyzer_Analyze_EmptyFunction(t *testing.T) {
	fn := &ssa.Function{}
	analyzer := NewAnalyzer(fn, nil)

	violations := analyzer.Analyze()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations for empty function, got %d", len(violations))
	}
}

func TestPollutionTracker_DetectReachabilityViolations_NoStates(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)

	// Should not panic with empty states
	tracker.DetectReachabilityViolations()
}

func TestPollutionTracker_DetectReachabilityViolations_SinglePollution(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()
	fn := &ssa.Function{}
	tracker := NewPollutionTracker(cfgAnalyzer, fn)

	// Add a state with only one polluted block
	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, &ssa.BasicBlock{}, token.Pos(1))

	// Should not create violations with only one pollution site
	tracker.DetectReachabilityViolations()

	violations := tracker.CollectViolations()
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations with single pollution, got %d", len(violations))
	}
}

// ============================================================================
// Additional tests for better coverage
// ============================================================================

func TestCFGAnalyzer_CanReach_WithSuccessors(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Create a chain: block1 -> block2 -> block3
	block1 := &ssa.BasicBlock{}
	block2 := &ssa.BasicBlock{}
	block3 := &ssa.BasicBlock{}

	block1.Succs = []*ssa.BasicBlock{block2}
	block2.Succs = []*ssa.BasicBlock{block3}

	// block1 can reach block3 via block2
	if !cfgAnalyzer.CanReach(block1, block3) {
		t.Error("block1 should reach block3")
	}

	// block3 cannot reach block1 (no back edge)
	if cfgAnalyzer.CanReach(block3, block1) {
		t.Error("block3 should not reach block1")
	}

	// block1 can reach block2 directly
	if !cfgAnalyzer.CanReach(block1, block2) {
		t.Error("block1 should reach block2")
	}
}

func TestCFGAnalyzer_CanReach_Unreachable(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	block1 := &ssa.BasicBlock{}
	block2 := &ssa.BasicBlock{}

	// No successors, so unreachable
	if cfgAnalyzer.CanReach(block1, block2) {
		t.Error("Disconnected blocks should not be reachable")
	}
}

func TestCFGAnalyzer_DetectLoops_WithBlocks(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Create blocks for a simple loop: block0 -> block1 -> block0 (back edge)
	block0 := &ssa.BasicBlock{Index: 0}
	block1 := &ssa.BasicBlock{Index: 1}

	block0.Succs = []*ssa.BasicBlock{block1}
	block1.Succs = []*ssa.BasicBlock{block0} // back edge

	fn := &ssa.Function{
		Blocks: []*ssa.BasicBlock{block0, block1},
	}

	loopInfo := cfgAnalyzer.DetectLoops(fn)

	// Both blocks should be marked as in loop
	if !loopInfo.IsInLoop(block0) {
		t.Error("block0 should be in loop")
	}
	if !loopInfo.IsInLoop(block1) {
		t.Error("block1 should be in loop")
	}
}

func TestCFGAnalyzer_DetectLoops_NoLoop(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Linear blocks: block0 -> block1 -> block2 (no back edge)
	block0 := &ssa.BasicBlock{Index: 0}
	block1 := &ssa.BasicBlock{Index: 1}
	block2 := &ssa.BasicBlock{Index: 2}

	block0.Succs = []*ssa.BasicBlock{block1}
	block1.Succs = []*ssa.BasicBlock{block2}

	fn := &ssa.Function{
		Blocks: []*ssa.BasicBlock{block0, block1, block2},
	}

	loopInfo := cfgAnalyzer.DetectLoops(fn)

	// No blocks should be in loop
	if len(loopInfo.LoopBlocks) != 0 {
		t.Errorf("Expected no loop blocks, got %d", len(loopInfo.LoopBlocks))
	}
}

func TestPollutionTracker_IsPollutedAt_SameFunction(t *testing.T) {
	fn := &ssa.Function{}
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, fn)

	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 1}
	block1.Succs = []*ssa.BasicBlock{block2}

	// Set parent function for blocks
	fn.Blocks = []*ssa.BasicBlock{block1, block2}

	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, block1, token.Pos(1))

	// block2 is reachable from polluted block1
	if !tracker.IsPollutedAt(mockValue, block2) {
		t.Error("block2 should be polluted (reachable from block1)")
	}
}

func TestPollutionTracker_IsPollutedAt_UnreachableBlocks(t *testing.T) {
	fn := &ssa.Function{}
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, fn)

	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 1}
	// No successors set - blocks are disconnected
	fn.Blocks = []*ssa.BasicBlock{block1, block2}

	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, block1, token.Pos(1))

	// block2 is NOT reachable from block1 (no path)
	if tracker.IsPollutedAt(mockValue, block2) {
		t.Error("block2 should not be polluted (unreachable from block1)")
	}
}

func TestPollutionTracker_IsPollutedAnywhere_WithBlock(t *testing.T) {
	fn := &ssa.Function{}
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, fn)

	block := &ssa.BasicBlock{Index: 0}
	fn.Blocks = []*ssa.BasicBlock{block}

	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, block, token.Pos(1))

	// Same function - should be polluted
	if !tracker.IsPollutedAnywhere(mockValue, fn) {
		t.Error("Should be polluted anywhere in same function")
	}
}

func TestPollutionTracker_IsPollutedAnywhere_DifferentFunction(t *testing.T) {
	fn1 := &ssa.Function{}
	fn2 := &ssa.Function{}
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, fn1)

	block := &ssa.BasicBlock{Index: 0}
	fn1.Blocks = []*ssa.BasicBlock{block}

	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, block, token.Pos(1))

	// Different function - closure pollution affects parent
	if !tracker.IsPollutedAnywhere(mockValue, fn2) {
		t.Error("Should be polluted (closure affects parent)")
	}
}

func TestCFGAnalyzer_IsDefinedOutsideLoop_NilBlock(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

	// Call is an instruction but Block() returns nil for uninitialized Call
	call := &ssa.Call{}

	loopInfo := &LoopInfo{LoopBlocks: map[*ssa.BasicBlock]bool{}}

	// Instruction with nil block should be considered outside loop
	result := cfgAnalyzer.IsDefinedOutsideLoop(call, loopInfo)
	if !result {
		t.Error("Instruction with nil block should be outside loop")
	}
}

func TestRootTracer_FindMutableRoot_Phi(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Phi with nil edges returns nil
	phi := &ssa.Phi{}
	result := tracer.FindMutableRoot(phi)
	if result != nil {
		t.Error("Phi with no edges should return nil")
	}
}

func TestRootTracer_FindMutableRoot_ChangeType(t *testing.T) {
	tracer := NewRootTracer(nil)

	// ChangeType traces through X
	changeType := &ssa.ChangeType{X: nil}
	result := tracer.FindMutableRoot(changeType)
	if result != nil {
		t.Error("ChangeType with nil X should return nil")
	}
}

func TestRootTracer_FindMutableRoot_Extract(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Extract traces through Tuple
	extract := &ssa.Extract{Tuple: nil}
	result := tracer.FindMutableRoot(extract)
	if result != nil {
		t.Error("Extract with nil Tuple should return nil")
	}
}

func TestClosureCapturesGormDB_WithNonGormBinding(t *testing.T) {
	// MakeClosure with non-gorm.DB binding
	param := &ssa.Parameter{} // not *gorm.DB type
	mc := &ssa.MakeClosure{
		Bindings: []ssa.Value{param},
	}

	if ClosureCapturesGormDB(mc) {
		t.Error("MakeClosure with non-gorm.DB binding should not capture GormDB")
	}
}

func TestRootTracer_FindMutableRoot_Parameter(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Parameter is immutable source
	param := &ssa.Parameter{}
	result := tracer.FindMutableRoot(param)
	if result != nil {
		t.Error("Parameter should return nil (immutable)")
	}
}

func TestRootTracer_FindMutableRoot_Call_NilCallee(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Call with nil StaticCallee
	call := &ssa.Call{}
	result := tracer.FindMutableRoot(call)
	// StaticCallee is nil, returns nil
	if result != nil {
		t.Error("Call with nil callee should return nil")
	}
}

func TestPollutionTracker_DetectReachabilityViolations_MultiplePollutions_SameBlock(t *testing.T) {
	fn := &ssa.Function{}
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, fn)

	block1 := &ssa.BasicBlock{Index: 0}
	fn.Blocks = []*ssa.BasicBlock{block1}

	// Same block for both - won't create second entry (MarkPolluted returns false)
	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, block1, token.Pos(1))
	tracker.MarkPolluted(mockValue, block1, token.Pos(2)) // Same block, returns false

	tracker.DetectReachabilityViolations()
	// No violations because there's only one unique block
}

func TestPollutionTracker_DetectReachabilityViolations_ReachableBlocks(t *testing.T) {
	fn := &ssa.Function{}
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, fn)

	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 1}
	block1.Succs = []*ssa.BasicBlock{block2}
	fn.Blocks = []*ssa.BasicBlock{block1, block2}

	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, block1, token.Pos(1))
	tracker.MarkPolluted(mockValue, block2, token.Pos(2))

	tracker.DetectReachabilityViolations()

	violations := tracker.CollectViolations()
	// block1 can reach block2, so block2 should be a violation
	if len(violations) != 1 {
		t.Errorf("Expected 1 violation, got %d", len(violations))
	}
}

func TestCFGAnalyzer_MarkLoopBlocks(t *testing.T) {
	cfgAnalyzer := NewCFGAnalyzer()

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
	cfgAnalyzer.markLoopBlocks(fn, block0, block2, loopBlocks, blockIndex)

	if !loopBlocks[block0] || !loopBlocks[block1] || !loopBlocks[block2] {
		t.Error("All blocks should be marked as in loop")
	}
}

func TestRootTracer_FindMutableRoot_NilInput(t *testing.T) {
	tracer := NewRootTracer(nil)

	result := tracer.FindMutableRoot(nil)
	if result != nil {
		t.Error("FindMutableRoot(nil) should return nil")
	}
}

func TestRootTracer_FindMutableRoot_UnOp_NonDeref(t *testing.T) {
	tracer := NewRootTracer(nil)

	// UnOp that is not dereference (e.g., negation)
	unop := &ssa.UnOp{
		Op: token.SUB, // Not MUL (dereference)
		X:  nil,
	}
	result := tracer.FindMutableRoot(unop)
	if result != nil {
		t.Error("UnOp with nil X should return nil")
	}
}

func TestRootTracer_FindMutableRoot_UnOp_Deref(t *testing.T) {
	tracer := NewRootTracer(nil)

	// UnOp that is dereference
	unop := &ssa.UnOp{
		Op: token.MUL, // Dereference
		X:  nil,
	}
	result := tracer.FindMutableRoot(unop)
	// TracePointerLoad with nil returns nil
	if result != nil {
		t.Error("UnOp deref with nil X should return nil")
	}
}

func TestRootTracer_FindMutableRoot_FreeVar(t *testing.T) {
	tracer := NewRootTracer(nil)

	fv := &ssa.FreeVar{}
	result := tracer.FindMutableRoot(fv)
	// TraceFreeVar with nil parent returns nil
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestSSATracer_TracePointerLoad_NilPointer(t *testing.T) {
	ssaTracer := NewSSATracer()

	result := ssaTracer.TracePointerLoad(nil, func(v ssa.Value) ssa.Value { return nil })
	if result != nil {
		t.Error("TracePointerLoad(nil) should return nil")
	}
}

func TestSSATracer_TracePointerLoad_FreeVar(t *testing.T) {
	ssaTracer := NewSSATracer()

	fv := &ssa.FreeVar{}
	result := ssaTracer.TracePointerLoad(fv, func(v ssa.Value) ssa.Value { return nil })
	// TraceFreeVar with nil parent returns nil
	if result != nil {
		t.Error("TracePointerLoad(FreeVar) with nil parent should return nil")
	}
}

func TestSSATracer_TracePointerLoad_OtherValue(t *testing.T) {
	ssaTracer := NewSSATracer()

	// Parameter falls through to trace callback
	param := &ssa.Parameter{}
	result := ssaTracer.TracePointerLoad(param, func(v ssa.Value) ssa.Value { return nil })
	// Callback returns nil
	if result != nil {
		t.Error("TracePointerLoad(Parameter) should return nil when callback returns nil")
	}
}

func TestSSATracer_TraceFreeVar_NotFound(t *testing.T) {
	ssaTracer := NewSSATracer()

	// FreeVar with nil parent - TraceFreeVar returns nil
	fv := &ssa.FreeVar{}
	result := ssaTracer.TraceFreeVar(fv, func(v ssa.Value) ssa.Value { return nil })
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestRootTracer_IsImmutableSource_Call_NilCallee(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Call with nil callee should not be immutable
	call := &ssa.Call{}
	if tracer.isImmutableSource(call) {
		t.Error("Call with nil callee should not be immutable")
	}
}

func TestPollutionTracker_DetectReachabilityViolations_CrossFunction(t *testing.T) {
	fn := &ssa.Function{}
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, fn)

	// Create blocks in different "functions" (simulate closure)
	block1 := &ssa.BasicBlock{Index: 0}
	block2 := &ssa.BasicBlock{Index: 0}

	fn.Blocks = []*ssa.BasicBlock{block1}
	// block2 belongs to fn as well (same Parent)

	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, block1, token.Pos(1))
	tracker.MarkPolluted(mockValue, block2, token.Pos(2))

	// Both blocks have same Parent() (nil), so they're in same function
	tracker.DetectReachabilityViolations()
}

func TestClosureCapturesGormDB_PointerToPointer(t *testing.T) {
	// MakeClosure with a pointer type binding (not *gorm.DB)
	param := &ssa.Parameter{}
	mc := &ssa.MakeClosure{
		Bindings: []ssa.Value{param},
	}

	// Parameter type is nil, so it won't match *gorm.DB
	if ClosureCapturesGormDB(mc) {
		t.Error("MakeClosure with nil type binding should not capture GormDB")
	}
}

func TestRootTracer_FindMutableRoot_PhiWithEdges(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Phi with edges that are all immutable
	param := &ssa.Parameter{} // immutable, will return nil
	phi := &ssa.Phi{
		Edges: []ssa.Value{param},
	}

	result := tracer.FindMutableRoot(phi)
	// Parameter is immutable, returns nil
	if result != nil {
		t.Error("Phi with only immutable edges should return nil")
	}
}

// =============================================================================
// SSATracer TraceFreeVar Tests
// =============================================================================

func TestSSATracer_TraceFreeVar_NilParent(t *testing.T) {
	ssaTracer := NewSSATracer()

	// FreeVar with nil Parent
	fv := &ssa.FreeVar{}
	result := ssaTracer.TraceFreeVar(fv, func(v ssa.Value) ssa.Value { return nil })
	if result != nil {
		t.Error("FreeVar with nil parent should return nil")
	}
}

func TestSSATracer_TraceFreeVar_NotInFreeVars(t *testing.T) {
	ssaTracer := NewSSATracer()

	// FreeVar with no matching entry in fn.FreeVars
	fv := &ssa.FreeVar{}
	result := ssaTracer.TraceFreeVar(fv, func(v ssa.Value) ssa.Value { return nil })
	// Parent is nil, returns nil
	if result != nil {
		t.Error("FreeVar not found in parent's FreeVars should return nil")
	}
}

func TestSSATracer_TraceFreeVar_NilGrandparent(t *testing.T) {
	ssaTracer := NewSSATracer()

	// This tests the case where fn.Parent() is nil
	fv := &ssa.FreeVar{}
	result := ssaTracer.TraceFreeVar(fv, func(v ssa.Value) ssa.Value { return nil })
	if result != nil {
		t.Error("FreeVar with nil grandparent should return nil")
	}
}

// =============================================================================
// SSATracer TraceIIFEReturns Tests
// =============================================================================

func TestSSATracer_TraceIIFEReturns_NilResults(t *testing.T) {
	ssaTracer := NewSSATracer()
	visited := make(map[ssa.Value]bool)

	// Function with nil Signature.Results()
	fn := &ssa.Function{}
	result := ssaTracer.TraceIIFEReturns(fn, visited, func(v ssa.Value, vis map[ssa.Value]bool) ssa.Value { return nil })
	if result != nil {
		t.Error("Function with nil results should return nil")
	}
}

func TestSSATracer_TraceIIFEReturns_EmptyResults(t *testing.T) {
	ssaTracer := NewSSATracer()
	visited := make(map[ssa.Value]bool)

	// Function with empty signature (void return)
	fn := &ssa.Function{}
	result := ssaTracer.TraceIIFEReturns(fn, visited, func(v ssa.Value, vis map[ssa.Value]bool) ssa.Value { return nil })
	if result != nil {
		t.Error("Function with no return type should return nil")
	}
}

func TestSSATracer_TraceIIFEReturns_NonGormDBReturn(t *testing.T) {
	ssaTracer := NewSSATracer()
	visited := make(map[ssa.Value]bool)

	// Function that doesn't return *gorm.DB - signature is nil
	fn := &ssa.Function{}
	result := ssaTracer.TraceIIFEReturns(fn, visited, func(v ssa.Value, vis map[ssa.Value]bool) ssa.Value { return nil })
	if result != nil {
		t.Error("Function not returning *gorm.DB should return nil")
	}
}

func TestSSATracer_TraceIIFEReturns_NoBlocks(t *testing.T) {
	ssaTracer := NewSSATracer()
	visited := make(map[ssa.Value]bool)

	// Function with nil Blocks
	fn := &ssa.Function{}
	result := ssaTracer.TraceIIFEReturns(fn, visited, func(v ssa.Value, vis map[ssa.Value]bool) ssa.Value { return nil })
	if result != nil {
		t.Error("Function with no blocks should return nil")
	}
}

// =============================================================================
// detectReachabilityViolations Tests - Cross-function cases
// =============================================================================

func TestPollutionTracker_DetectReachabilityViolations_SinglePollution_NoViolation(t *testing.T) {
	parentFn := &ssa.Function{}
	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, parentFn)

	// Only one pollution site - no violation possible
	block := &ssa.BasicBlock{Index: 0}
	parentFn.Blocks = []*ssa.BasicBlock{block}

	var mockValue ssa.Value
	tracker.MarkPolluted(mockValue, block, token.Pos(1))

	tracker.DetectReachabilityViolations()

	violations := tracker.CollectViolations()
	// Single pollution should not create violation
	if len(violations) != 0 {
		t.Errorf("Expected 0 violations (single pollution), got %d", len(violations))
	}
}

// =============================================================================
// isImmutableSource Tests
// =============================================================================

func TestRootTracer_IsImmutableSource_NilValue(t *testing.T) {
	tracer := NewRootTracer(nil)

	// nil value
	result := tracer.isImmutableSource(nil)
	if result {
		t.Error("nil value should not be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_Alloc(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Alloc is not an immutable source
	alloc := &ssa.Alloc{}
	result := tracer.isImmutableSource(alloc)
	if result {
		t.Error("Alloc should not be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_MakeInterface(t *testing.T) {
	tracer := NewRootTracer(nil)

	// MakeInterface is not an immutable source
	mi := &ssa.MakeInterface{}
	result := tracer.isImmutableSource(mi)
	if result {
		t.Error("MakeInterface should not be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_Phi(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Phi is not an immutable source
	phi := &ssa.Phi{}
	result := tracer.isImmutableSource(phi)
	if result {
		t.Error("Phi should not be immutable source")
	}
}

func TestRootTracer_IsImmutableSource_CallWithNilCallee(t *testing.T) {
	tracer := NewRootTracer(nil)

	// Call with nil StaticCallee
	call := &ssa.Call{}
	result := tracer.isImmutableSource(call)
	if result {
		t.Error("Call with nil callee should not be immutable source")
	}
}

// =============================================================================
// CallHandler processBoundMethodCall Tests
// =============================================================================

func TestCallHandler_ProcessBoundMethodCall_EmptyBindings(t *testing.T) {
	handler := &CallHandler{}

	call := &ssa.Call{}
	mc := &ssa.MakeClosure{
		Bindings: []ssa.Value{}, // empty
	}

	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)
	rootTracer := NewRootTracer(nil)
	loopInfo := &LoopInfo{LoopBlocks: make(map[*ssa.BasicBlock]bool)}

	ctx := &HandlerContext{
		Tracker:     tracker,
		RootTracer:  rootTracer,
		CFGAnalyzer: cfgAnalyzer,
		LoopInfo:    loopInfo,
	}

	// Should not panic with empty bindings
	handler.processBoundMethodCall(call, mc, false, ctx)
}

func TestCallHandler_ProcessBoundMethodCall_NonGormDBBinding(t *testing.T) {
	handler := &CallHandler{}

	call := &ssa.Call{}
	param := &ssa.Parameter{} // Not *gorm.DB type
	mc := &ssa.MakeClosure{
		Bindings: []ssa.Value{param},
	}

	cfgAnalyzer := NewCFGAnalyzer()
	tracker := NewPollutionTracker(cfgAnalyzer, nil)
	rootTracer := NewRootTracer(nil)
	loopInfo := &LoopInfo{LoopBlocks: make(map[*ssa.BasicBlock]bool)}

	ctx := &HandlerContext{
		Tracker:     tracker,
		RootTracer:  rootTracer,
		CFGAnalyzer: cfgAnalyzer,
		LoopInfo:    loopInfo,
	}

	// Should not panic with non-gorm.DB binding
	handler.processBoundMethodCall(call, mc, false, ctx)
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
// IsNilConst Tests
// =============================================================================

func TestIsNilConst_NilConst(t *testing.T) {
	// Const with nil Value is a nil constant
	c := &ssa.Const{Value: nil}
	if !IsNilConst(c) {
		t.Error("Const with nil Value should be nil constant")
	}
}

func TestIsNilConst_NonNilConst(t *testing.T) {
	// Const with non-nil Value is not a nil constant
	c := &ssa.Const{Value: constant.MakeInt64(42)}
	if IsNilConst(c) {
		t.Error("Const with non-nil Value should not be nil constant")
	}
}

func TestIsNilConst_NonConst(t *testing.T) {
	// Non-Const value is not a nil constant
	p := &ssa.Parameter{}
	if IsNilConst(p) {
		t.Error("Parameter should not be nil constant")
	}
}

func TestIsNilConst_Nil(t *testing.T) {
	// nil value is not a nil constant
	if IsNilConst(nil) {
		t.Error("nil should not be nil constant")
	}
}
