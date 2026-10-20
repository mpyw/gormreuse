package tracer

import (
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
)

// stateSource has merges whose edges do and do not set Statement state that
// a Session drops (#344).
const stateSource = `package shape

import "gorm.io/gorm"

type User struct{ Age int }

func assignOnEdge(db *gorm.DB, c bool) *gorm.DB {
	var q *gorm.DB
	if c {
		q = db.Model(&User{}).Assign(User{Age: 1})
	} else {
		q = db.Model(&User{})
	}
	return q
}

func plainEdges(db *gorm.DB, c bool) *gorm.DB {
	var q *gorm.DB
	if c {
		q = db.Model(&User{}).Where("a")
	} else {
		q = db.Model(&User{})
	}
	return q
}
`

// TestSessionDropsStateThroughPhi: a merge carries the state of any of its
// edges.
func TestSessionDropsStateThroughPhi(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureSource(t, stateSource)
	tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})
	for name, want := range map[string]bool{"assignOnEdge": true, "plainEdges": false} {
		phi := stateFirstPhi(t, funcs[name])
		if got := tr.SessionDropsState(phi); got != want {
			t.Errorf("%s: SessionDropsState(%s) = %v, want %v", name, phi, got, want)
		}
	}
}

// stateFirstPhi returns the first Phi of fn.
func stateFirstPhi(t *testing.T, fn *ssa.Function) *ssa.Phi {
	t.Helper()
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if phi, ok := instr.(*ssa.Phi); ok {
				return phi
			}
		}
	}
	t.Fatalf("%s has no Phi", fn.Name())
	return nil
}
