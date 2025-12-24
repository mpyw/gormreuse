package ssa

import (
	"go/token"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// PollutionTracker Tests
// =============================================================================

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
// isFunctionDescendantOf Tests
// =============================================================================

func TestIsFunctionDescendantOf_NilFn(t *testing.T) {
	if isFunctionDescendantOf(nil, &ssa.Function{}) {
		t.Error("nil fn should not be descendant of anything")
	}
}

func TestIsFunctionDescendantOf_NilAncestor(t *testing.T) {
	if isFunctionDescendantOf(&ssa.Function{}, nil) {
		t.Error("nothing should be descendant of nil")
	}
}

func TestIsFunctionDescendantOf_BothNil(t *testing.T) {
	if isFunctionDescendantOf(nil, nil) {
		t.Error("nil should not be descendant of nil")
	}
}

func TestIsFunctionDescendantOf_SameFunction(t *testing.T) {
	fn := &ssa.Function{}
	if !isFunctionDescendantOf(fn, fn) {
		t.Error("function should be descendant of itself")
	}
}

func TestIsFunctionDescendantOf_NoParent(t *testing.T) {
	fn := &ssa.Function{}
	ancestor := &ssa.Function{}
	// fn has no Parent, so it's not a descendant of ancestor
	if isFunctionDescendantOf(fn, ancestor) {
		t.Error("function with no parent should not be descendant of unrelated function")
	}
}
