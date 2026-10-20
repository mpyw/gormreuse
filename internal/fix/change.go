package fix

import (
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// changedElsewhere reports whether a fix could make an in-place change to
// v's root disappear, other than that of a discarded statement on its
// variable (see findReassignedChains). That is so when:
//   - a use hands the root to other code, such as a helper, a closure or a
//     container, which may change it in place (see UsageInfo.Escapes), and
//     a later use may see the change (see changeSeenAfterEscape);
//   - the Error, RowsAffected or Statement field is read from the root, or
//     from a chain on it that runs no query. Such a read sees what the calls
//     on the root left in it: the errors of its queries, or the conditions of
//     a chain whose only result is the field, as in
//     `if err := q.Where("a").Error; ...`;
//   - a go statement hands the root to a call, as in `go applyTenant(q)`. The
//     goroutine may change it while the uses after the statement run;
//   - the root is stored into a struct field or an element that other code
//     can reach, as in `s := &Svc{db: q}; s.apply()` (see
//     holderChangesRoot, #310).
//
// A Session at the root makes each of them see a copy (#243).
//
//declscope:shared // generator.go's Generate withholds the fix with it
func (g *Generator) changedElsewhere(v pollution.Violation) bool {
	ordered := usesOrdered(v)
	for _, use := range v.AllUses {
		if use.Escapes && changeSeenAfterEscape(g.cfg, tracer.RootBlock(v.Root), use, v.AllUses, ordered, g.tracer.FuncVars()) {
			return true
		}
	}
	if g.holderChangesRoot(v, ordered) {
		return true
	}
	for _, f := range withClosures(v.Root.Parent()) {
		loopInfo := g.cfg.DetectLoops(f)
		for _, block := range f.Blocks {
			for _, instr := range block.Instrs {
				switch instr := instr.(type) {
				case *ssa.FieldAddr:
					if isGormStateField(instr) && g.carriesRootChanges(v.Root, instr.X, loopInfo) {
						return true
					}
				case *ssa.Go:
					if g.goChangesRoot(instr, v.Root, loopInfo) {
						return true
					}
				}
			}
		}
	}
	return false
}

// goChangesRoot reports whether instr, a go statement, hands root to code
// that may change it in place. The receiver of a *gorm.DB finisher, as in
// `go q.Count(nil)`, is not handed over: the query is a use of the root like
// any other, and runs on a copy once the root has a Session, as with the
// closure form `go func() { q.Count(nil) }()` (#312). Nor is that of a
// Session-family call, which copies it. The receiver of a chain
// method, as in `go q.Where("a")`, is: the goroutine adds the condition to
// the root in place, and a go statement cannot be reassigned to keep it.
func (g *Generator) goChangesRoot(instr *ssa.Go, root ssa.Value, loopInfo *cfg.LoopInfo) bool {
	site := g.tracer.Site(instr)
	queryOrCopy := site.IsGormMethod() && (isGormFinisher(site.Method()) || typeutil.IsImmutableReturningBuiltin(site.Method()))
	for i, arg := range instr.Call.Args {
		if queryOrCopy && site.ArgParam(i) < 0 {
			continue
		}
		if gormArg, ok := pollutionsource.UnwrapGormDB(arg); ok && g.tracer.FindMutableRoot(gormArg, loopInfo) == root {
			return true
		}
	}
	return false
}

// changeSeenAfterEscape reports whether code may see what escape, a use that
// hands the root to other code, changes in place. A use of the root may, when
// it is reachable from escape or escape runs again in a loop. So may a use of
// the result of a call that returns a *gorm.DB, such as `q = withTenant(q)`:
// the result may be the root itself. Without the order of every use (see
// usesOrdered) the change is assumed to be seen. An escape that is the last
// use, as in `q.Find(&a); render(q)`, keeps its fix. def is the block that
// defines the root: an escape runs again when the checker's cfg.Repeats says
// so. vars resolves calls through package-level function variables.
//
//declscope:shared // holder.go's holderStoreChangesRoot takes each point where a struct holding the root is reached as an escape
func changeSeenAfterEscape(a *cfg.Analyzer, def *ssa.BasicBlock, escape pollution.UsageInfo, uses []pollution.UsageInfo, ordered bool, vars callsite.FuncVars) bool {
	if !ordered || useRepeatsAfter(a, escape.Block, def) || changeSeenThroughResult(escape, vars) {
		return true
	}
	return slices.ContainsFunc(uses, func(use pollution.UsageInfo) bool {
		if use.Block == escape.Block {
			return use.Pos > escape.Pos
		}
		return a.CanReach(escape.Block, use.Block)
	})
}

// changeSeenThroughResult reports whether use is an argument of a call that
// returns a *gorm.DB, alone or in a tuple, other than a *gorm.DB method. A
// method such as base.Model(q) returns a chain on its receiver, also when it
// is called as a method value.
func changeSeenThroughResult(use pollution.UsageInfo, vars callsite.FuncVars) bool {
	return slices.ContainsFunc(use.Block.Instrs, func(instr ssa.Instruction) bool {
		call, ok := instr.(*ssa.Call)
		return ok && call.Pos() == use.Pos && typeutil.HasGormDBResult(call.Type()) &&
			!callsite.SiteOf(call, vars).IsGormMethod()
	})
}

// carriesRootChanges reports whether x, a *gorm.DB whose state field is read,
// is root or a chain on root of *gorm.DB methods that runs no query (see
// changeCarryingRoots).
func (g *Generator) carriesRootChanges(root, x ssa.Value, loopInfo *cfg.LoopInfo) bool {
	return slices.Contains(g.changeCarryingRoots(x, loopInfo), root)
}

// changeCarryingRoots returns the roots whose in-place changes x, a *gorm.DB,
// carries: its own root, and the roots of the receivers of a chain of
// *gorm.DB methods that runs no query. The tracer makes each call its own
// root, so the chain is walked down its receivers. The walk stops at a
// finisher, whose result holds its own query's outcome, as in
// q.Where("a").Find(&x).Error, and at a Session-family call, whose result is
// a copy.
//
//declscope:shared // holder.go's holderStores keys the struct stores by them
func (g *Generator) changeCarryingRoots(x ssa.Value, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for {
		if root := g.tracer.FindMutableRoot(x, loopInfo); root != nil {
			roots = append(roots, root)
		}
		call, ok := x.(*ssa.Call)
		if !ok {
			return roots
		}
		site := g.tracer.Site(call)
		if !site.IsGormMethod() || isGormFinisher(site.Method()) || typeutil.IsImmutableReturningBuiltin(site.Method()) {
			return roots
		}
		x = site.Recv
	}
}
