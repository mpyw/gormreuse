package ssa

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// ClosureCapturesGormDB Tests
// =============================================================================

func TestClosureCapturesGormDB_Empty(t *testing.T) {
	// MakeClosure with no bindings
	mc := &ssa.MakeClosure{}
	if ClosureCapturesGormDB(mc) {
		t.Error("Empty MakeClosure should not capture GormDB")
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

// =============================================================================
// CallHandler ProcessBoundMethodCall Tests
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
