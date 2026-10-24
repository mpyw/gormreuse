package tracer

import (
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
)

func TestClosureCapturesGormDB(t *testing.T) {
	t.Parallel()
	_, all := loadFixtureProgram(t)

	var sawCapturing bool
	for fn := range all {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if mc, ok := instr.(*ssa.MakeClosure); ok && NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{}).ClosureCapturesGormDB(mc) {
					sawCapturing = true
				}
			}
		}
	}
	if !sawCapturing {
		t.Error("expected at least one gorm-capturing closure among the fixtures")
	}
}
