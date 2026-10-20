package tracer

import (
	"go/types"
	"maps"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// ClosureCapturesGormDB checks if a closure captures *gorm.DB values.
//
// Used to determine if a closure needs recursive analysis. A closure
// that doesn't capture *gorm.DB can be skipped for efficiency.
//
// Looks through pointer chains (*gorm.DB, **gorm.DB, ...) and struct fields, so
// a closure capturing a struct that holds a *gorm.DB is analyzed too (#181).
func ClosureCapturesGormDB(mc *ssa.MakeClosure) bool {
	return slices.ContainsFunc(mc.Bindings, func(binding ssa.Value) bool {
		return closureHoldsGormDB(binding.Type(), make(map[types.Type]bool))
	})
}

// isClosureResultStored checks if a closure call's result is stored (assigned to
// a variable) rather than directly chained in an IIFE pattern.
//
// Returns true (stored) for patterns like:
//   - `q, err := closureFunc()` (flows to Extract → stored)
//   - `q := closureFunc()` (flows to Store/Alloc → stored)
//   - `if cond { q = closureFunc() }` (flows to Phi → stored)
//   - `q := closureFunc().Where("x")` (chain eventually stored → stored)
//
// Returns false (chained IIFE) for patterns like:
//   - `closureFunc().Find(nil)` (chain ends with terminal, never stored)
//
//declscope:shared // root.go's traceCall and traceAll keep a stored closure result as its own root
func (t *RootTracer) isClosureResultStored(call *ssa.Call) bool {
	return t.isClosureResultStoredRecursive(call, make(map[*ssa.Call]bool))
}

func (t *RootTracer) isClosureResultStoredRecursive(call *ssa.Call, visited map[*ssa.Call]bool) bool {
	if visited[call] {
		return false
	}
	visited[call] = true

	refs := call.Referrers()
	if refs == nil {
		return false
	}

	for _, ref := range *refs {
		switch user := ref.(type) {
		case *ssa.Extract:
			// Multi-return extraction → stored
			return true

		case *ssa.Phi:
			// Flows to Phi → stored (conditional assignment)
			return true

		case *ssa.Store:
			// Store to Alloc → stored (variable assignment)
			if _, ok := user.Addr.(*ssa.Alloc); ok {
				return true
			}

		case *ssa.MakeInterface:
			// Value converted to interface{} (e.g., for variadic args) → stored
			// This happens when passing *gorm.DB to methods with interface{} args
			return true

		case *ssa.Call:
			// A method value call is the call of its method.
			site := t.Site(user)
			if !site.IsGormMethod() {
				continue
			}

			// Check if our result is receiver of this gorm method
			if site.Recv == call {
				// Our result is receiver - check if THAT call is stored
				if t.isClosureResultStoredRecursive(user, visited) {
					return true
				}
			} else {
				// Our result is passed as argument (e.g., condition.Or(ourResult))
				// This is effectively "stored" - the value is consumed by another call
				return true
			}
		}
	}

	return false
}

// closureHoldsGormDB reports whether a captured value of type t holds a
// *gorm.DB the tracer can follow: t is *gorm.DB or gorm.DB, a pointer to such a
// type (**gorm.DB, ...), or a struct with such a field, embedded or not (#181). seen stops recursive
// struct types.
//
// Unlike typeutil's containsGormDB, this does NOT check:
//   - interface{} (would cause false positives in SSA analysis)
//   - slices, maps, channels (SSA handles these differently)
func closureHoldsGormDB(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true

	if typeutil.IsGormDB(t) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		return closureHoldsGormDB(u.Elem(), seen)
	case *types.Struct:
		for field := range u.Fields() {
			if closureHoldsGormDB(field.Type(), seen) {
				return true
			}
		}
	}
	return false
}

// traceClosureReturns traces through an immediately invoked function expression.
//
// IIFE pattern:
//
//	_ = func() *gorm.DB {
//	    return q.Where("y")   // ← trace this return value
//	}().Find(nil)
//
// This allows treating the entire IIFE chain as a single branch from q,
// rather than two separate branches (IIFE internal + Find external).
//
// We clone the visited map to avoid polluting the parent's tracking state.
//
//declscope:shared // root.go's traceCall sends IIFEs here
func (t *RootTracer) traceClosureReturns(fn *ssa.Function, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 || !typeutil.IsGormDB(results.At(0).Type()) {
		return nil
	}

	// Collect ALL roots from all return statements
	var allRoots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			// Clone visited map to isolate closure's tracing state
			retVisited := maps.Clone(visited)
			if root := t.trace(ret.Results[0], retVisited, loopInfo); root != nil {
				allRoots = append(allRoots, root)
			}
		}
	}

	if len(allRoots) == 0 {
		return nil
	}

	// If all roots are the same, return that root
	firstRoot := allRoots[0]
	allSame := true
	for _, root := range allRoots[1:] {
		if root != firstRoot {
			allSame = false
			break
		}
	}

	if allSame {
		return firstRoot
	}

	// Multiple different roots - return nil to signal caller should use call itself
	// This happens with conditional returns like:
	//   func() *gorm.DB { if cond { return q1 } else { return q2 } }()
	return nil
}

// traceAllClosureReturns is the multi-root version of traceClosureReturns.
// It collects roots from ALL return statements in the closure.
//
//declscope:shared // root.go's traceAll sends IIFEs here
func (t *RootTracer) traceAllClosureReturns(fn *ssa.Function, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	if fn.Signature == nil {
		return nil
	}
	results := fn.Signature.Results()
	if results == nil || results.Len() == 0 || !typeutil.IsGormDB(results.At(0).Type()) {
		return nil
	}

	var allRoots []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok || len(ret.Results) == 0 {
				continue
			}
			retSearch := newAllRootsSearch(maps.Clone(search.visited))
			roots := t.traceAll(ret.Results[0], retSearch, loopInfo)
			allRoots = append(allRoots, roots...)
		}
	}
	return allRoots
}

// closureInvocationCalls returns the plain calls of mc, the closure of a
// function literal, or nil when mc is used in any other way: deferred,
// spawned, stored or passed on. Only then are the calls all the places it runs.
//
//declscope:shared // store.go reads a captured variable at the closure's calls
func closureInvocationCalls(mc *ssa.MakeClosure) []*ssa.Call {
	var calls []*ssa.Call
	for _, ref := range *mc.Referrers() {
		call, ok := ref.(*ssa.Call)
		if !ok || call.Call.Value != ssa.Value(mc) {
			return nil
		}
		calls = append(calls, call)
	}
	return calls
}

// closureCallOwnsRoots reports whether call, a call of a function literal
// whose returns trace to roots, is a mutable root of its own rather than
// sharing them with the closure's other calls (#251). That is so when the
// closure may run more than once and each run builds a fresh chain:
//
//	base := func() *gorm.DB { return db.Model(&User{}) } // db immutable
//	base().Where("c").Find(nil)
//	base().Where("d").Find(nil)  // a new Statement, not a second branch
//
// Each root must be a gorm method call in the closure whose result is only
// returned, so no use inside the closure shares it with the caller's, and the
// chain it ends must start from immutable values. A chain that starts from a
// captured mutable root, a parameter or a call of another function is shared
// by every run, so it keeps its roots.
//
// cfg.Repeats counts the runs (#249).
//
//declscope:shared // root.go's traceCall and traceAll keep a closure factory's call as its own root
func (t *RootTracer) closureCallOwnsRoots(call *ssa.Call, roots []ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) bool {
	// A call through a captured local variable runs the closure it holds (#304).
	mc, ok := t.FuncVars().LocalValue(call.Call.Value).(*ssa.MakeClosure)
	if !ok || !t.closureRunsRepeatedly(mc) {
		return false
	}
	fn, _ := mc.Fn.(*ssa.Function)
	seen := make(map[*ssa.Call]bool)
	for _, root := range roots {
		c, ok := root.(*ssa.Call)
		if !ok || !closureResultOnlyReturned(c) || !t.closureChainIsFresh(c, fn, visited, loopInfo, seen) {
			return false
		}
	}
	return true
}

// closureRunsRepeatedly reports whether the closure mc may run more than once
// for one run of the MakeClosure, as cfg.Repeats counts it (#249): at two
// sites that may both run, or at a site in a loop that does not create it.
func (t *RootTracer) closureRunsRepeatedly(mc *ssa.MakeClosure) bool {
	fn, ok := mc.Fn.(*ssa.Function)
	if !ok || len(fn.Blocks) == 0 {
		return false
	}
	return t.cfgs.RepeatsOf(fn).RepeatsAfter(fn.Blocks[0], mc.Block())
}

// closureResultOnlyReturned reports whether every use of c's result is a
// return.
func closureResultOnlyReturned(c *ssa.Call) bool {
	for _, ref := range *c.Referrers() {
		if _, ok := ref.(*ssa.Return); !ok {
			return false
		}
	}
	return true
}

// closureChainIsFresh reports whether c, a call in fn, is a gorm method call
// whose *gorm.DB operands are immutable or come from such calls in fn, fresh
// on each run of fn. seen holds the calls already found fresh.
func (t *RootTracer) closureChainIsFresh(c *ssa.Call, fn *ssa.Function, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo, seen map[*ssa.Call]bool) bool {
	if seen[c] {
		return true
	}
	site := t.Site(c)
	if c.Parent() != fn || !site.IsGormMethod() {
		return false
	}
	seen[c] = true
	operands := site.Args
	if site.Recv != nil {
		operands = append([]ssa.Value{site.Recv}, operands...)
	}
	for _, op := range operands {
		// A gorm method takes no struct of handles: its other parameters
		// are interfaces, whose subqueries are only read (#185).
		if !typeutil.IsGormDB(op.Type()) {
			continue
		}
		for _, root := range t.traceAll(op, newAllRootsSearch(maps.Clone(visited)), loopInfo) {
			if t.IsImmutableCall(root) {
				continue
			}
			rc, ok := root.(*ssa.Call)
			if !ok || !t.closureChainIsFresh(rc, fn, visited, loopInfo, seen) {
				return false
			}
		}
	}
	return true
}
