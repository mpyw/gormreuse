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
//     a later use may see the change (see changeSeenAfterEscape). A helper
//     that only runs queries on it, or a recursive call that the fix's own
//     body Session isolates, does not count (see handoffKeepsChanges, #347).
//     noDiscarded says that the fix reassigns no discarded statement;
//   - the Error, RowsAffected or Statement field is read from the root, or
//     from a chain on it that runs no query. Such a read sees what the calls
//     on the root left in it: the errors of its queries, or the conditions of
//     a chain whose only result is the field, as in
//     `if err := q.Where("a").Error; ...`;
//   - a go statement hands the root to a call, as in `go applyTenant(q)`. The
//     goroutine may change it while the uses after the statement run;
//   - a defer statement changes the root in place, as `defer applyTenant(q)`
//     and `defer q.Where("a")` do, and another deferred call may see it:
//     the root has another use in a defer statement, or the statement runs
//     more than once. Deferred calls run after every other use, so only a
//     deferred call can see such a change, and without the order they run in
//     any other one is taken to. `defer q.Find(nil); defer q.Where("a")`
//     would lose its condition (#309);
//   - the root is stored into a struct field or an element that other code
//     can reach, as in `s := &Svc{db: q}; s.apply()` (see
//     holderChangesRoot, #310).
//
// A Session at the root makes each of them see a copy (#243).
//
//declscope:shared // generator.go's Generate withholds the fix with it
func (g *Generator) changedElsewhere(v pollution.Violation, noDiscarded bool) bool {
	ordered := usesOrdered(v)
	for _, use := range v.AllUses {
		if use.Escapes && changeSeenAfterEscape(g.cfg, tracer.RootBlock(v.Root), use, v.AllUses, ordered, g.tracer.FuncVars()) && !g.handoffKeepsChanges(v, use, noDiscarded) {
			return true
		}
	}
	if g.holderChangesRoot(v, ordered) {
		return true
	}
	deferUses, deferChanges, deferRepeats := 0, 0, false
	for _, f := range withClosures(v.Root.Parent()) {
		stmts := g.changeStatementsOf(f)
		if stmts.stateReads[v.Root] {
			return true
		}
		for _, stmt := range stmts.byRoot[v.Root] {
			switch {
			case stmt.isGo:
				if stmt.changes {
					return true
				}
			case stmt.changes:
				deferUses++
				deferChanges++
				deferRepeats = deferRepeats || useRepeatsAfter(g.cfg, stmt.block, tracer.RootBlock(v.Root))
			default:
				deferUses++
			}
		}
	}
	return deferChanges > 0 && (deferUses > 1 || deferRepeats)
}

// changeStatements is what changedElsewhere reads of one function for every
// root at once: the roots whose state field the function reads, and the go
// and defer statements that use each root. A function with n roots and n such
// statements is scanned once, not once per root (#350).
//
//declscope:shared // generator.go's Generator memoizes it per function
type changeStatements struct {
	//declscope:private
	stateReads map[ssa.Value]bool
	//declscope:private
	byRoot map[ssa.Value][]changeStatement
}

// changeStatement is a go or defer statement that uses a root, and whether it
// may change the root in place (see statementRootChanges).
type changeStatement struct {
	block   *ssa.BasicBlock
	isGo    bool
	changes bool
}

// changeStatementsOf returns the go and defer statements and the state reads of
// f, keyed by root, building them on first use.
func (g *Generator) changeStatementsOf(f *ssa.Function) *changeStatements {
	if stmts, ok := g.statements[f]; ok {
		return stmts
	}
	stmts := &changeStatements{stateReads: make(map[ssa.Value]bool), byRoot: make(map[ssa.Value][]changeStatement)}
	loopInfo := g.cfg.DetectLoops(f)
	for _, block := range f.Blocks {
		for _, instr := range block.Instrs {
			switch instr := instr.(type) {
			case *ssa.FieldAddr:
				if isGormStateField(instr) {
					for _, root := range g.changeCarryingRoots(instr.X, loopInfo) {
						stmts.stateReads[root] = true
					}
				}
			case *ssa.Go, *ssa.Defer:
				_, isGo := instr.(*ssa.Go)
				changes := g.statementRootChanges(instr.(ssa.CallInstruction), loopInfo)
				for _, root := range changes.roots {
					stmts.byRoot[root] = append(stmts.byRoot[root], changeStatement{block: block, isGo: isGo, changes: changes.changes[root]})
				}
			}
		}
	}
	if g.statements == nil {
		g.statements = make(map[*ssa.Function]*changeStatements)
	}
	g.statements[f] = stmts
	return stmts
}

// statementChanges are the roots a go or defer statement uses, in the order
// of its arguments, and whether it may change each in place.
type statementChanges struct {
	roots   []ssa.Value
	changes map[ssa.Value]bool
}

// statementRootChanges returns the roots instr, a go or defer statement,
// uses, and whether it hands each to code that may change it in place. The
// receiver of a *gorm.DB finisher, as in `go q.Count(nil)`, is not handed
// over: the query is a use of the root like any other, and runs on a copy
// once the root has a Session, as with the closure form
// `go func() { q.Count(nil) }()` (#312). Nor is that of a Session-family
// call, which copies it. The receiver of a chain method, as in
// `go q.Where("a")`, is: the call adds the condition to the root in place,
// and a go or defer statement cannot be reassigned to keep it.
func (g *Generator) statementRootChanges(instr ssa.CallInstruction, loopInfo *cfg.LoopInfo) statementChanges {
	site := g.tracer.Site(instr)
	queryOrCopy := site.IsGormMethod() && (typeutil.IsFinisher(site.Method()) || typeutil.IsImmutableReturningBuiltin(site.Method()))
	changes := statementChanges{changes: make(map[ssa.Value]bool)}
	for i, arg := range instr.Common().Args {
		gormArg, ok := pollutionsource.UnwrapGormDB(arg)
		if !ok {
			continue
		}
		root := g.tracer.FindMutableRoot(gormArg, loopInfo)
		if root == nil {
			continue
		}
		if _, seen := changes.changes[root]; !seen {
			changes.roots = append(changes.roots, root)
		}
		changes.changes[root] = changes.changes[root] || !queryOrCopy || site.ArgParam(i) >= 0
	}
	return changes
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
		if !site.IsGormMethod() || typeutil.IsFinisher(site.Method()) || typeutil.IsImmutableReturningBuiltin(site.Method()) {
			return roots
		}
		x = site.Recv
	}
}
