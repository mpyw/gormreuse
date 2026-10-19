// Package scopeswarn warns about Session()-family results returned from Scopes
// callbacks (TEMPORARY — GORM bug workaround). analyzer.go in package
// internal drives this check.
package scopeswarn

import (
	"cmp"
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// =============================================================================
// Scopes Session() Warning (TEMPORARY — GORM bug workaround)
// =============================================================================
//
// GORM has a bug where a Scopes callback that returns a Session()/
// WithContext()/Debug() result corrupts the InstanceSet/InstanceGet
// bookkeeping and leaks the transaction:
//
//   - Issue: https://github.com/go-gorm/gorm/issues/7592
//   - Patch: https://github.com/go-gorm/gorm/pull/7593 (closed unmerged)
//
// gorm runs the create/update/delete callbacks on the value the scope returns.
// BeginTransaction stores its flag with InstanceSet, which clones the Statement
// when that value has clone>0, so CommitOrRollbackTransaction never sees the
// flag. Session/WithContext/Debug return clone>0; every other chain method
// returns clone==0. So only a Session-family result that IS the return value
// leaks, whichever *gorm.DB it was called on (#196,
// e2e/internal/scopes_session_test.go).
//
// **Removal condition**: once the upstream fix ships in a tagged GORM
// release and gormreuse drops support for older versions, delete this
// entire package and the call site in RunSSA. The remaining detection
// (Scopes callback parameter reuse) is handled by Phase 1 alone.
//
// Why this lives in its own package: it is independent of pollution
// tracking, has a known sunset, and would otherwise pollute the
// general orchestration code in analyzer.go.

// Warning represents a warning about a Session()-family result returned
// from a Scopes callback.
type Warning struct {
	Pos     token.Pos
	Message string
}

// maxCallDepth bounds how deep Validate follows helper calls.
const maxCallDepth = 16

// Validate warns about each Session()/WithContext()/Debug() call whose result
// a Scopes callback returns. callbacks is the set of functions passed to Scopes
// (callback.CollectScopesOnlyCallbacks). The returned value is followed through
// Phi edges, variables, captured variables, and the returns of helpers and
// closures the callback calls. The warnings are sorted by position, one per
// call. analyzer.go (package internal) drives this check.
func Validate(callbacks map[*ssa.Function]bool, skip func(*ssa.Function) bool) []Warning {
	found := make(map[*ssa.Call]bool)
	for fn := range callbacks {
		if skip(fn) {
			continue
		}
		t := &returnTracer{found: found, seen: make(map[frameValue]bool)}
		t.returns(&frame{fn: fn}, 0)
	}

	warnings := make([]Warning, 0, len(found))
	for call := range found {
		warnings = append(warnings, Warning{Pos: call.Pos(), Message: message(callsite.SiteOf(call, nil).Method())})
	}
	slices.SortFunc(warnings, func(a, b Warning) int { return cmp.Compare(a.Pos, b.Pos) })
	return warnings
}

// message returns the warning text for a Session-family method name.
func message(name string) string {
	if name == "Session" {
		return "Session() result returned from Scopes callback causes transaction leak (GORM bug)"
	}
	return name + "() result returned from Scopes callback causes transaction leak (calls Session internally)"
}

// frame is one function on the helper call stack. args are the caller's
// arguments, so a helper's parameter resolves to what the caller passed.
type frame struct {
	fn     *ssa.Function
	args   []ssa.Value
	caller *frame
	depth  int
}

// onStack reports whether fn is already being traced, which stops recursion.
func (f *frame) onStack(fn *ssa.Function) bool {
	for ; f != nil; f = f.caller {
		if f.fn == fn {
			return true
		}
	}
	return false
}

// enclosing returns the frame of the function that binds fv: the one on the
// stack when the closure was called from it, otherwise a frame without
// arguments, where a parameter is not followed.
func (f *frame) enclosing(fv *ssa.FreeVar) *frame {
	parent := fv.Parent().Parent()
	for g := f; g != nil; g = g.caller {
		if g.fn == parent {
			return g
		}
	}
	return &frame{fn: parent, caller: f, depth: f.depth}
}

type frameValue struct {
	f *frame
	v ssa.Value
}

// returnTracer finds the Session-family calls that a function's return value
// may be.
type returnTracer struct {
	found map[*ssa.Call]bool
	seen  map[frameValue]bool
}

// returns traces result idx of every return statement in f.fn.
func (t *returnTracer) returns(f *frame, idx int) {
	for _, block := range f.fn.Blocks {
		for _, instr := range block.Instrs {
			if ret, ok := instr.(*ssa.Return); ok && idx < len(ret.Results) {
				t.value(f, ret.Results[idx])
			}
		}
	}
}

// value traces v, a value of f.fn, back to the calls that may produce it.
func (t *returnTracer) value(f *frame, v ssa.Value) {
	if v == nil || t.seen[frameValue{f, v}] {
		return
	}
	t.seen[frameValue{f, v}] = true

	switch x := v.(type) {
	case *ssa.Call:
		t.call(f, x, 0)
	case *ssa.Extract:
		if call, ok := x.Tuple.(*ssa.Call); ok {
			t.call(f, call, x.Index)
		}
	case *ssa.Phi:
		for _, e := range x.Edges {
			t.value(f, e)
		}
	case *ssa.Parameter:
		if f.caller == nil {
			return // the callback's own input: what gorm passed in
		}
		if i := slices.Index(f.fn.Params, x); i >= 0 && i < len(f.args) {
			t.value(f.caller, f.args[i])
		}
	case *ssa.FreeVar:
		t.value(f.enclosing(x), tracer.FreeVarBinding(x))
	case *ssa.UnOp:
		if x.Op == token.MUL {
			t.load(f, x.X)
		}
	case *ssa.ChangeType:
		t.value(f, x.X)
	}
}

// load traces the values stored at addr (a local or a captured local).
func (t *returnTracer) load(f *frame, addr ssa.Value) {
	switch a := addr.(type) {
	case *ssa.Alloc:
		for _, sv := range tracer.AllocStoredValues(a) {
			t.value(f, sv)
		}
	case *ssa.FreeVar:
		t.load(f.enclosing(a), tracer.FreeVarBinding(a))
	}
}

// call traces result idx of call. A Session-family *gorm.DB method is a
// finding; any other *gorm.DB method returns a clone==0 value and ends the
// trace; a helper or closure is followed into its return statements.
//
// A method value call is read as a call of its wrapper, which is followed like
// a helper.
func (t *returnTracer) call(f *frame, call *ssa.Call, idx int) {
	site := callsite.SiteOf(call, nil).StaticSite()
	// Only a *gorm.DB method is matched by name, so that an unrelated
	// package's identically-named method doesn't trigger GORM-specific
	// diagnostics.
	if site.IsGormMethod() {
		switch site.Method() {
		case "Session", "WithContext", "Debug":
			if idx == 0 {
				t.found[call] = true
			}
		}
		return
	}
	callee := site.Callee
	if callee == nil || len(callee.Blocks) == 0 || f.depth >= maxCallDepth || f.onStack(callee) {
		return
	}
	t.returns(&frame{fn: callee, args: call.Call.Args, caller: f, depth: f.depth + 1}, idx)
}
