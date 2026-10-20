package tracer

import (
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
)

// aliasLoopShapes are searches whose graph has cycles through loops, the case
// mayAlias used to answer true without looking (#300).
const aliasLoopShapes = `package shape

import "gorm.io/gorm"

func rebuild(db *gorm.DB, xs []string) {
	q := db.Model(nil)
	for _, x := range xs {
		q = q.Where(x)
	}
	q.Count(nil)
	q.Find(nil)
}

func rebuildTwice(db *gorm.DB, xs []string) {
	q := db.Model(nil)
	for _, x := range xs {
		q = q.Where(x)
	}
	for _, x := range xs {
		for _, y := range xs {
			q = q.Where(x).Or(y)
		}
	}
	q.Find(nil)
}

func copyInLoop(db *gorm.DB, xs []string) {
	q := db.Model(nil)
	for _, x := range xs {
		q = q.Session(&gorm.Session{}).Where(x)
	}
	q.Find(nil)
}

func copyCarried(db *gorm.DB, xs []string) {
	q := db.Model(nil)
	for _, x := range xs {
		q = q.Where(x).Session(&gorm.Session{})
	}
	q.Find(nil)
}

func subqueryInLoop(db *gorm.DB, xs []string) {
	sub := db.Model(nil)
	q := db.Model(nil)
	for _, x := range xs {
		q = q.Where(x, sub)
		sub = sub.Where(x)
	}
	q.Find(nil)
}
`

// TestMayAliasLeastFixedPoint checks mayAlias against a plain search of
// everything a collectAliasedRoots search reaches, on every receiver and Phi
// of the fixtures and of aliasLoopShapes. The answers share one RootTracer
// per function, so later queries read what earlier ones left in the memo.
func TestMayAliasLeastFixedPoint(t *testing.T) {
	t.Parallel()
	fixtures, _ := loadFixtureProgram(t)
	shapes := loadFixtureSource(t, aliasLoopShapes)
	loops := cfg.NewAnalyzer()

	var answers [2]int
	check := func(fn *ssa.Function) {
		tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})
		loopInfo := loops.DetectLoops(fn)
		for _, v := range aliasQueryValues(fn) {
			for _, copies := range []bool{true, false} {
				want := aliasReachesRoot(tr, aliasedRootsKey{v, rootsLoopInfo(loopInfo), copies}, map[aliasedRootsKey]bool{})
				if got := tr.mayAlias(v, rootsLoopInfo(loopInfo), copies); got != want {
					t.Errorf("%s: mayAlias(%v, copies=%t) = %t, want %t", fn, v, copies, got, want)
				}
				if copies {
					if got := len(tr.FindAliasedRoots(v, loopInfo)) > 0; got != want {
						t.Errorf("%s: FindAliasedRoots(%v) found roots = %t, want %t", fn, v, got, want)
					}
				}
				if want {
					answers[1]++
				} else {
					answers[0]++
				}
			}
		}
	}
	for _, funcs := range []map[string]*ssa.Function{fixtures, shapes} {
		for _, fn := range funcs {
			check(fn)
			for _, anon := range fn.AnonFuncs {
				check(anon)
			}
		}
	}
	if answers[0] == 0 || answers[1] == 0 {
		t.Errorf("want both answers, got %d false and %d true", answers[0], answers[1])
	}

	// A loop that only rebuilds the query reads no copy: its cycle is
	// answered false, and so is every search on it. A copy that the next
	// call forks is not read either.
	for _, name := range []string{"rebuild", "rebuildTwice", "copyInLoop"} {
		fn := shapes[name]
		tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})
		recv := fixtureGormCallReceiver(fn, "Find")
		if tr.mayAlias(recv, loops.DetectLoops(fn), true) {
			t.Errorf("%s: the rebuilt query may alias, want not", name)
		}
		for key, may := range tr.aliases {
			if may {
				t.Errorf("%s: search %v (copies=%t) answered true", name, key.v, key.copies)
			}
		}
	}
	for _, name := range []string{"copyCarried", "subqueryInLoop"} {
		fn := shapes[name]
		tr := NewRootTracer(nil, nil, nil, nil, nil, nil, callsite.FuncVars{})
		if !tr.mayAlias(fixtureGormCallReceiver(fn, "Find"), loops.DetectLoops(fn), true) {
			t.Errorf("%s: the query reads a copy or an argument, want may alias", name)
		}
	}
}

// aliasQueryValues returns the receivers of fn's calls and its Phis.
func aliasQueryValues(fn *ssa.Function) []ssa.Value {
	var values []ssa.Value
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			switch instr := instr.(type) {
			case *ssa.Phi:
				values = append(values, instr)
			case *ssa.Call:
				if len(instr.Call.Args) > 0 && !instr.Call.IsInvoke() {
					values = append(values, instr.Call.Args[0])
				}
			}
		}
	}
	return values
}

// aliasReachesRoot reports whether the search key, or one it reaches, adds a
// root at its own step. It is the definition mayAlias computes, as a plain
// search that visits each search once; only the answer for the key it starts
// from is meaningful.
func aliasReachesRoot(tr *RootTracer, key aliasedRootsKey, seen map[aliasedRootsKey]bool) bool {
	if seen[key] {
		return false
	}
	seen[key] = true
	for _, candidate := range tr.FindAllMutableRoots(key.v, key.loopInfo) {
		_, site, ok := tr.aliasStep(candidate)
		if !ok {
			continue
		}
		isCopy := key.copies && tr.aliasSharesStatement(site)
		if isCopy && tr.FindMutableRoot(site.Recv, key.loopInfo) != nil {
			return true
		}
		for _, arg := range pollutionsource.ReadOnlyGormArgs(site) {
			if tr.FindMutableRoot(arg, key.loopInfo) != nil || aliasReachesRoot(tr, aliasedRootsKey{arg, key.loopInfo, true}, seen) {
				return true
			}
		}
		if aliasReachesRoot(tr, aliasedRootsKey{site.Recv, key.loopInfo, isCopy}, seen) {
			return true
		}
	}
	return false
}
