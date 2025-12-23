package ssa

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

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
