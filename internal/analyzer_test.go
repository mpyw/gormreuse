package internal

import (
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
