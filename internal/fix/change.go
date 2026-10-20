package fix

import (
	"go/token"
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

// finisherErrorChanges reports whether a Session at the root would change
// which error, or which RowsAffected, v's queries report (#372). On a mutable
// root a query's result is the root itself, so its Error carries the errors
// of the queries that ran on the root before it, and gorm skips a query once
// Error is set. Take this code:
//
//	q := db.Model(&Order{ID: id})
//	q.Update("name", "new") // fails
//	return q.Update("id", id).Error
//
// The second Update is skipped, and the function returns the error of the
// first. With a Session at q, each Update runs on its own copy, so the
// second one runs and the function returns nil. In a Transaction callback
// that turns a rollback into a commit.
//
// It holds when a use runs a query whose Error nothing reads, and a later
// read, or the same query again in a loop, passes the error on: it reads a
// field of a result, returns it, or hands it to code that may (see
// changeErrorsOfUse). A use whose chain is a discarded statement, such as
// q.Find(&x), passes nothing on. A result kept where other code may read it
// later, such as a struct field, is taken to pass it on after every query.
// So is a use that AllUses does not list (see pollution.Violation.Hidden),
// such as a deferred call or a query on a Session copy, which is also taken
// to run before every read. A use in a closure is ordered by its position.
//
//declscope:shared // generator.go's Generate withholds the fix with it
func (g *Generator) finisherErrorChanges(v pollution.Violation) bool {
	if changes, ok := g.errorChanges[v.Root]; ok {
		return changes
	}
	changes := g.computeFinisherErrorChanges(v)
	if g.errorChanges == nil {
		g.errorChanges = make(map[ssa.Value]bool)
	}
	g.errorChanges[v.Root] = changes
	return changes
}

// computeFinisherErrorChanges is finisherErrorChanges without the memo: the
// violations of one root share their uses, so a root reused n times is
// checked once, not n times (#350).
func (g *Generator) computeFinisherErrorChanges(v pollution.Violation) bool {
	def := tracer.RootBlock(v.Root)
	var unread, passed []changePoint
	for _, use := range v.AllUses {
		errs := g.changeErrorsOfUse(v.Root, use)
		unread = append(unread, errs.unread...)
		passed = append(passed, errs.passed...)
	}
	for _, use := range v.Hidden {
		errs := g.changeErrorsOfUse(v.Root, use)
		if len(errs.unread) > 0 {
			unread = append(unread, changePoint{})
		}
		if len(errs.passed) > 0 {
			passed = append(passed, changePoint{})
		}
	}
	for _, first := range unread {
		repeats := first.block == nil || def == nil || useRepeatsAfter(g.cfg, first.block, def)
		if slices.ContainsFunc(passed, func(later changePoint) bool {
			switch {
			case repeats || later.block == nil:
				return true
			case later.value == first.value:
				// A query's own result: its error is its own.
				return false
			case later.block == first.block || later.block.Parent() != first.block.Parent():
				return later.pos > first.pos
			}
			return g.cfg.CanReach(first.block, later.block)
		}) {
			return true
		}
	}
	return false
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

// changeUseErrors is what a use does with the error on a mutable root (see
// finisherErrorChanges).
type changeUseErrors struct {
	// unread are the queries whose result is discarded, or whose fields
	// other than Error are the only ones read. Their error stays on the root
	// for the queries after them.
	unread []changePoint
	// passed are where a result is read, returned or handed to other code,
	// so that an error an earlier query left on the root is seen.
	passed []changePoint
}

// changePoint is where a query or a read of its result runs. A zero block
// stands for a point that may run anywhere.
type changePoint struct {
	block *ssa.BasicBlock
	pos   token.Pos
	value ssa.Value // the result it is of
}

// changeErrorsOfUse returns what use, a use of root, does with the error of
// its chain: the *gorm.DB methods that start at the use's call, each called
// on the result of the one before, as in q.Where("a").Find(&x).Error (see
// changeErrorsOfValue). A use moved to the call site of its closure (#68) is
// read at its own call. A use that is an argument passes the error on only
// when the call it is passed to may (see changeRefPassesError).
func (g *Generator) changeErrorsOfUse(root ssa.Value, use pollution.UsageInfo) changeUseErrors {
	pos := use.Pos
	if use.Method != "" {
		pos = use.Call
	}
	instr := changeCallAt(root, use.Block, pos)
	if instr == nil {
		return changeUseErrors{passed: []changePoint{{}}}
	}
	if use.Method == "" {
		if !g.changeRefPassesError(instr, nil) {
			return changeUseErrors{}
		}
		return changeUseErrors{passed: []changePoint{{block: instr.Block(), pos: instr.Pos()}}}
	}
	call, ok := instr.(*ssa.Call)
	if !ok {
		// A go or defer statement has no result.
		return changeUseErrors{}
	}
	return g.changeErrorsOfValue(call, changeWalk{seen: make(map[ssa.Value]bool)})
}

// changeCallAt returns the call instruction at pos: in block, or in a
// closure of root's function.
func changeCallAt(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) ssa.CallInstruction {
	find := func(b *ssa.BasicBlock) ssa.CallInstruction {
		for _, instr := range b.Instrs {
			if call, ok := instr.(ssa.CallInstruction); ok && call.Pos() == pos {
				return call
			}
		}
		return nil
	}
	if call := find(block); call != nil {
		return call
	}
	if root.Parent() == nil {
		return nil
	}
	for _, f := range withClosures(root.Parent()) {
		for _, b := range f.Blocks {
			if call := find(b); call != nil {
				return call
			}
		}
	}
	return nil
}

// changeErrorsDepth bounds how many values changeErrorsOfValue follows a
// result through. A deeper result is taken to pass the error on.
const changeErrorsDepth = 16

// changeErrorsOfValue returns what the code that reads v, a *gorm.DB that is
// a mutable root at run time, does with the error it holds. query says
// whether a call before v ran a query. v is followed:
//   - into each *gorm.DB method called on it, as the receiver. Each runs on
//     the same Statement, so its result holds the same error;
//   - out of a closure that is only called in place, as in
//     `_ = func() *gorm.DB { return q.Where("a") }().Find(nil)`, into the
//     results of its calls;
//   - into the loads of a local variable it is stored in, and into a
//     closure that captures it. What a closure does is taken to run
//     anywhere (far): the order of closures is not followed.
//
// A Phi that takes v passes nothing on by itself: the uses of the Phi are
// uses of the root too. A return from the function passes it on where it
// returns. A store anywhere else, such as a struct field, passes it on
// anywhere. A query's error is unread when its result is discarded, or when
// only a field other than Error is read from it.
func (g *Generator) changeErrorsOfValue(v ssa.Value, walk changeWalk) changeUseErrors {
	if walk.depth >= changeErrorsDepth {
		return changeUseErrors{passed: []changePoint{{}}}
	}
	if walk.seen[v] {
		return changeUseErrors{}
	}
	walk.seen[v] = true
	if call, ok := v.(*ssa.Call); ok {
		if site := g.tracer.Site(call); site.IsGormMethod() && typeutil.IsFinisher(site.Method()) {
			walk.query = true
		}
	}
	var errs changeUseErrors
	errorRead, kept := false, false
	for _, ref := range *v.Referrers() {
		r := g.changeErrorsOfRef(ref, v, walk)
		errs.unread = append(errs.unread, r.unread...)
		errs.passed = append(errs.passed, r.passed...)
		errorRead = errorRead || r.errorRead
		kept = kept || r.kept
	}
	if walk.query && !errorRead && !kept {
		point := changePoint{}
		if instr, ok := v.(ssa.Instruction); ok && !walk.far {
			point = changePoint{block: instr.Block(), pos: changeValuePos(v), value: v}
		}
		errs.unread = append(errs.unread, point)
	}
	return errs
}

// changeWalk is the state of changeErrorsOfValue along one path: whether a
// query ran on the value before, how deep the walk is, whether the code is
// taken to run anywhere (far), and the values already walked.
type changeWalk struct {
	query bool
	depth int
	far   bool
	seen  map[ssa.Value]bool
}

// changeRefErrors is what one referrer of a value does with its error (see
// changeErrorsOfRef).
type changeRefErrors struct {
	changeUseErrors
	errorRead bool // it reads the value's Error
	kept      bool // it keeps the value, or follows it into other code
}

// changeErrorsOfRef returns what ref, a referrer of v, does with the error v
// holds (see changeErrorsOfValue).
func (g *Generator) changeErrorsOfRef(ref ssa.Instruction, v ssa.Value, walk changeWalk) changeRefErrors {
	var r changeRefErrors
	follow := func(next ssa.Value, far bool) {
		more := g.changeErrorsOfValue(next, changeWalk{query: walk.query, depth: walk.depth + 1, far: walk.far || far, seen: walk.seen})
		r.unread = append(r.unread, more.unread...)
		r.passed = append(r.passed, more.passed...)
	}
	pass := func(anywhere bool) {
		if anywhere || walk.far || ref.Block() == nil {
			r.passed = append(r.passed, changePoint{})
			return
		}
		pos := ref.Pos()
		if !pos.IsValid() {
			pos = changeValuePos(v)
		}
		r.passed = append(r.passed, changePoint{block: ref.Block(), pos: pos, value: v})
	}
	switch ref := ref.(type) {
	case *ssa.DebugRef:
		return r
	case *ssa.FieldAddr:
		pass(false)
		r.errorRead = gormFieldName(ref) == "Error"
		return r
	case *ssa.Call:
		if site := g.tracer.Site(ref); site.IsGormMethod() && site.Recv == v {
			follow(ref, false)
		} else if g.changeRefPassesError(ref, v) {
			pass(false)
		}
	case *ssa.Return:
		if results, ok := changeInlineResults(ref, v); ok {
			for _, result := range results {
				follow(result, false)
			}
		} else {
			pass(ref.Parent().Parent() != nil)
		}
	case *ssa.Store:
		alloc, ok := changeVariableOf(ref.Addr).(*ssa.Alloc)
		if !ok || ref.Val != v {
			if g.changeRefPassesError(ref, v) {
				pass(true)
			}
			break
		}
		reads, escapes := changeVariableReads(alloc)
		if escapes {
			pass(true)
		}
		for _, loaded := range reads {
			follow(loaded, loaded.Parent() != alloc.Parent() || ref.Parent() != alloc.Parent())
		}
	case *ssa.MakeClosure:
		for i, bound := range ref.Bindings {
			if bound == v {
				follow(ref.Fn.(*ssa.Function).FreeVars[i], true)
			}
		}
	case *ssa.Phi:
	default:
		if g.changeRefPassesError(ref, v) {
			pass(true)
		}
	}
	r.kept = true
	return r
}

// changeValuePos returns the position of v: that of the call whose result it
// is, for an element of a tuple.
func changeValuePos(v ssa.Value) token.Pos {
	if extract, ok := v.(*ssa.Extract); ok {
		return extract.Tuple.Pos()
	}
	return v.Pos()
}

// changeVariableOf returns the variable that addr, the address of a store,
// stands for: the one a closure captures, for a free variable.
func changeVariableOf(addr ssa.Value) ssa.Value {
	for {
		fv, ok := addr.(*ssa.FreeVar)
		if !ok {
			return addr
		}
		fn := fv.Parent()
		index := slices.Index(fn.FreeVars, fv)
		var bound ssa.Value
		for _, block := range fn.Parent().Blocks {
			for _, instr := range block.Instrs {
				if mc, isMC := instr.(*ssa.MakeClosure); isMC && mc.Fn == fn {
					bound = mc.Bindings[index]
				}
			}
		}
		if bound == nil {
			return addr
		}
		addr = bound
	}
}

// changeVariableReads returns the values that the loads of alloc, a local
// variable, read, in its function or a closure that captures it. escapes is
// set when its address goes anywhere else, as to a call.
func changeVariableReads(alloc *ssa.Alloc) (reads []ssa.Value, escapes bool) {
	var walk func(addr ssa.Value, depth int)
	walk = func(addr ssa.Value, depth int) {
		for _, ref := range *addr.Referrers() {
			switch ref := ref.(type) {
			case *ssa.DebugRef, *ssa.Store:
			case *ssa.UnOp:
				if ref.Op == token.MUL {
					reads = append(reads, ref)
				}
			case *ssa.MakeClosure:
				if depth >= changeErrorsDepth {
					escapes = true
					continue
				}
				for i, bound := range ref.Bindings {
					if bound == addr {
						walk(ref.Fn.(*ssa.Function).FreeVars[i], depth+1)
					}
				}
			default:
				escapes = true
			}
		}
	}
	walk(alloc, 0)
	return reads, escapes
}

// changeInlineResults returns the values that the calls of the closure that
// ret returns from take v from, when the closure is only called, by plain
// calls in the function that declares it: each call's result, or the element
// of its tuple at v's index. ok is false otherwise, as for a closure that is
// stored, passed on, or run by a go or defer statement.
func changeInlineResults(ret *ssa.Return, v ssa.Value) (results []ssa.Value, ok bool) {
	fn := ret.Parent()
	index := slices.Index(ret.Results, v)
	if index < 0 || fn.Parent() == nil {
		return nil, false
	}
	for _, block := range fn.Parent().Blocks {
		for _, instr := range block.Instrs {
			if mc, isMC := instr.(*ssa.MakeClosure); isMC && mc.Fn == fn {
				for _, r := range *mc.Referrers() {
					c, isCall := r.(*ssa.Call)
					if !isCall || c.Call.Value != mc || slices.Contains(c.Call.Args, ssa.Value(mc)) {
						return nil, false
					}
				}
				continue
			}
			for _, op := range instr.Operands(nil) {
				if *op != ssa.Value(fn) {
					continue
				}
				c, isCall := instr.(*ssa.Call)
				if !isCall || c.Call.Value != fn || slices.Contains(c.Call.Args, ssa.Value(fn)) {
					return nil, false
				}
			}
		}
	}
	for _, block := range fn.Parent().Blocks {
		for _, instr := range block.Instrs {
			c, isCall := instr.(*ssa.Call)
			if !isCall {
				continue
			}
			if mc, isMC := c.Call.Value.(*ssa.MakeClosure); (!isMC || mc.Fn != fn) && c.Call.Value != ssa.Value(fn) {
				continue
			}
			if len(ret.Results) == 1 {
				results = append(results, c)
				continue
			}
			for _, r := range *c.Referrers() {
				if extract, isExtract := r.(*ssa.Extract); isExtract && extract.Index == index {
					results = append(results, extract)
				}
			}
		}
	}
	return results, true
}

// changeRefPassesError reports whether ref, an instruction that takes a
// *gorm.DB, may pass on the error that the value holds. A call passes it on
// only through its own result, as in return helper(q): a helper that returns
// nothing, as in applyTenant(q, 7), and a go or defer statement have no
// result to carry it. The argument of a *gorm.DB method, such as the
// condition of q.Or(q.Where("a")), passes nothing on. Any instruction other
// than a call, such as a return or a store, is taken to pass it on. So is a
// *gorm.DB method that takes v, the value, as its receiver: it runs on the
// same Statement, and its result holds the same error.
func (g *Generator) changeRefPassesError(ref ssa.Instruction, v ssa.Value) bool {
	call, ok := ref.(ssa.CallInstruction)
	if !ok {
		calls, ok := changeVariadicCalls(ref)
		if !ok {
			return true
		}
		return slices.ContainsFunc(calls, func(call ssa.CallInstruction) bool { return g.changeRefPassesError(call, nil) })
	}
	if site := g.tracer.Site(call); site.IsGormMethod() {
		return v != nil && site.Recv == v
	}
	result := call.Value()
	return result != nil && callResultUsed(result)
}

// changeVariadicCalls returns the calls that ref, an instruction that is not
// a call, only hands its value to: the calls that take the interface a
// MakeInterface boxes it in, and the variadic calls whose argument array a
// Store puts it in, as `q.Or(q.Where("a"))` and `db.Where("id IN (?)", sub)`
// do. ok is false when the value may go anywhere else.
func changeVariadicCalls(ref ssa.Instruction) (calls []ssa.CallInstruction, ok bool) {
	switch ref := ref.(type) {
	case *ssa.MakeInterface:
		for _, r := range *ref.Referrers() {
			switch r := r.(type) {
			case *ssa.DebugRef:
			case ssa.CallInstruction:
				calls = append(calls, r)
			default:
				more, ok := changeVariadicCalls(r)
				if !ok {
					return nil, false
				}
				calls = append(calls, more...)
			}
		}
		return calls, true
	case *ssa.Store:
		idx, isIdx := ref.Addr.(*ssa.IndexAddr)
		if !isIdx {
			return nil, false
		}
		array, isAlloc := idx.X.(*ssa.Alloc)
		if !isAlloc {
			return nil, false
		}
		for _, r := range *array.Referrers() {
			switch r := r.(type) {
			case *ssa.IndexAddr:
			case *ssa.Slice:
				for _, sr := range *r.Referrers() {
					call, isCall := sr.(ssa.CallInstruction)
					if !isCall {
						return nil, false
					}
					calls = append(calls, call)
				}
			default:
				return nil, false
			}
		}
		return calls, true
	}
	return nil, false
}
