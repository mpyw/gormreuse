package tracer

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestClosureCapturesGormDB(t *testing.T) {
	t.Parallel()
	_, all := loadFixtureProgram(t)

	var sawCapturing bool
	for fn := range all {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if mc, ok := instr.(*ssa.MakeClosure); ok && ClosureCapturesGormDB(mc) {
					sawCapturing = true
				}
			}
		}
	}
	if !sawCapturing {
		t.Error("expected at least one gorm-capturing closure among the fixtures")
	}
}
