package fix

import (
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// handoffCache memoizes the finisher-only parameters of each function of the
// package (see HandoffFinisherOnlyParams).
//
//declscope:shared // generator.go's Generator holds it for the pass
type handoffCache struct {
	//declscope:private
	params map[*ssa.Function][]int
}

// handoffKeepsChanges reports whether escape, a use of v's root that hands
// it to a call, cannot leave a change in the root that a Session at the root
// would drop (#347). That is so when the call is a plain static call of a
// function that returns no *gorm.DB, and each *gorm.DB argument goes to a
// parameter that:
//   - is finisher-only (see HandoffFinisherOnlyParams). The callee's only in-place
//     effect on it is the pollution of its queries, which is the reuse the
//     fix removes;
//   - or is the reused parameter itself, passed to a recursive call of its
//     own function, and the fix reassigns no discarded statement (noDiscarded).
//     The fix rebinds the parameter to a Session at the top of the body, so
//     the callee changes only its own copy.
//
// No other argument may hold the root or run code: a struct or address that
// holds a *gorm.DB, a function, an interface or a type parameter keeps the
// escape. Any other call, a go or defer statement, a method value and a
// method expression keep it too.
//
//declscope:shared // change.go's changedElsewhere skips such an escape
func (g *Generator) handoffKeepsChanges(v pollution.Violation, escape pollution.UsageInfo, noDiscarded bool) bool {
	var call *ssa.Call
	for _, instr := range escape.Block.Instrs {
		if c, ok := instr.(*ssa.Call); ok && c.Pos() == escape.Pos {
			if call != nil {
				return false // two calls at one position: not told apart
			}
			call = c
		}
	}
	if call == nil {
		return false
	}
	site := g.tracer.Site(call)
	if site.Kind != callsite.SiteStatic || site.Declared == nil || site.IsGormMethod() || typeutil.HasGormDBResult(call.Type()) {
		return false
	}
	callee := origin.Declared(site.Declared)
	selfParam := -1
	if p, ok := v.Root.(*ssa.Parameter); ok && noDiscarded && origin.Declared(p.Parent()) == callee {
		selfParam = slices.Index(p.Parent().Params, p)
	}
	finisherOnly := g.HandoffFinisherOnlyParams(callee)
	recv := 0
	if callee.Signature.Recv() != nil {
		recv = 1
	}
	loopInfo := g.cfg.DetectLoops(call.Parent())
	for i, arg := range call.Common().Args {
		if typeutil.IsGormDB(arg.Type()) {
			param := site.ArgParam(i) + recv
			if param != selfParam && !slices.Contains(finisherOnly, param) {
				return false
			}
			continue
		}
		if handoffRunsCode(arg) || g.tracer.PointeeRoots(arg, call, loopInfo) != nil || len(g.tracer.GormDBFieldValues(arg, call)) > 0 {
			return false
		}
	}
	return true
}

// HandoffFinisherOnlyParams returns the indexes, the receiver first, of fn's
// *gorm.DB parameters that fn uses only to run queries (#347). Each use of
// such a parameter starts a chain of *gorm.DB methods that ends in a finisher
// or in Session, WithContext or Debug, as in `tx.Where("a").Find(&x).Error`.
// So fn does not:
//   - leave a discarded statement on it, such as `tx.Where("a")`;
//   - read its Error, RowsAffected or Statement field, other than the Error
//     or RowsAffected of a finisher's result;
//   - store it, return it, capture it in a closure, or hand it or a chain on
//     it to any other call;
//   - pass a function to a method of the chain, as Scopes, Connection and
//     Transaction take one, which could reach the parameter's Statement.
//
// It is empty for a function that returns a *gorm.DB, which could carry the
// parameter back. For a function of another package it is what that
// package's fact says (see facts.Func).
func (g *Generator) HandoffFinisherOnlyParams(fn *ssa.Function) []int {
	fn = origin.Declared(fn)
	if fn.Pkg == nil || fn.Pkg.Pkg != g.pass.Pkg {
		if g.importedFinisherOnly == nil {
			return nil
		}
		return g.importedFinisherOnly(fn)
	}
	c := &g.handoffs
	if params, ok := c.params[fn]; ok {
		return params
	}
	var params []int
	if fn.Blocks != nil && !typeutil.HasGormDBResult(fn.Signature.Results()) {
		for i, p := range fn.Params {
			if typeutil.IsGormDB(p.Type()) && g.handoffOnlyQueries(p, false, false) {
				params = append(params, i)
			}
		}
	}
	if c.params == nil {
		c.params = map[*ssa.Function][]int{}
	}
	c.params[fn] = params
	return params
}

// handoffOnlyQueries reports whether every use of v, a *gorm.DB, ends in a
// query or a copy (see HandoffFinisherOnlyParams). needsUse is set for a chain
// method's result, which a discarded statement leaves unused. queried is
// set for a finisher's result, whose Error and RowsAffected hold that query's
// outcome.
func (g *Generator) handoffOnlyQueries(v ssa.Value, needsUse, queried bool) bool {
	refs := v.Referrers()
	if refs == nil {
		return false
	}
	used := false
	for _, ref := range *refs {
		switch ref := ref.(type) {
		case *ssa.DebugRef:
			continue
		case *ssa.FieldAddr:
			if !queried || !handoffReadsOutcome(ref) {
				return false
			}
		case *ssa.Call:
			site := g.tracer.Site(ref)
			if site.Kind != callsite.SiteStatic || !site.IsGormMethod() || site.Recv != v || slices.Contains(site.Args, v) ||
				slices.ContainsFunc(site.Args, handoffPassesFunc) {
				return false
			}
			switch method := site.Method(); {
			case method == "Session" || method == "WithContext" || method == "Debug":
			case typeutil.IsFinisher(method):
				if typeutil.IsGormDB(ref.Type()) && !g.handoffOnlyQueries(ref, false, true) {
					return false
				}
			case typeutil.IsImmutableReturningBuiltin(method):
				return false // AddError changes the receiver; the others are not needed
			default:
				if !g.handoffOnlyQueries(ref, true, false) {
					return false
				}
			}
		default:
			return false
		}
		used = true
	}
	return used || !needsUse
}

// handoffReadsOutcome reports whether fa, the address of a field of a
// finisher's result, is only loaded, and is its Error or RowsAffected.
func handoffReadsOutcome(fa *ssa.FieldAddr) bool {
	if !isGormStateField(fa) {
		return false
	}
	name := fa.X.Type().Underlying().(*types.Pointer).Elem().Underlying().(*types.Struct).Field(fa.Field).Name()
	if name == "Statement" {
		return false
	}
	return !slices.ContainsFunc(*fa.Referrers(), func(r ssa.Instruction) bool {
		load, ok := r.(*ssa.UnOp)
		return !ok || load.X != fa
	})
}

// handoffPassesFunc reports whether arg, an argument of a *gorm.DB method,
// may be a function that gorm runs, as a scope or a callback: a function,
// directly, boxed in an interface or among the values of a variadic argument
// built at the call, or an interface whose value is not known. A struct such
// as a *gorm.Session is only read by gorm.
func handoffPassesFunc(arg ssa.Value) bool {
	switch arg := arg.(type) {
	case *ssa.Const:
		return false
	case *ssa.MakeInterface:
		return handoffPassesFunc(arg.X)
	case *ssa.Slice:
		if array, ok := arg.X.(*ssa.Alloc); ok {
			return slices.ContainsFunc(*array.Referrers(), func(r ssa.Instruction) bool {
				elem, ok := r.(*ssa.IndexAddr)
				if !ok {
					return r != arg
				}
				return slices.ContainsFunc(*elem.Referrers(), func(r ssa.Instruction) bool {
					store, ok := r.(*ssa.Store)
					return !ok || handoffPassesFunc(store.Val)
				})
			})
		}
	}
	switch u := arg.Type().Underlying().(type) {
	case *types.Signature, *types.Interface, *types.TypeParam:
		return true
	case *types.Slice:
		_, isFunc := u.Elem().Underlying().(*types.Signature)
		_, isIface := u.Elem().Underlying().(*types.Interface)
		return isFunc || isIface
	}
	return false
}

// handoffRunsCode reports whether arg may hold code: a function, an
// interface, or a type parameter, directly, boxed in an interface, or among
// the values of a variadic argument built at the call.
func handoffRunsCode(arg ssa.Value) bool {
	switch arg := arg.(type) {
	case *ssa.Const:
		return false
	case *ssa.MakeInterface:
		return handoffTypeRunsCode(arg.X.Type(), map[types.Type]bool{})
	case *ssa.Slice:
		if array, ok := arg.X.(*ssa.Alloc); ok {
			return slices.ContainsFunc(*array.Referrers(), func(r ssa.Instruction) bool {
				elem, ok := r.(*ssa.IndexAddr)
				if !ok {
					return r != arg
				}
				return slices.ContainsFunc(*elem.Referrers(), func(r ssa.Instruction) bool {
					store, ok := r.(*ssa.Store)
					return !ok || handoffRunsCode(store.Val)
				})
			})
		}
	}
	return handoffTypeRunsCode(arg.Type(), map[types.Type]bool{})
}

// handoffTypeRunsCode reports whether a value of type t may hold a function,
// an interface or a type parameter. Methods of t do not count: gorm runs a
// model's hooks on every query, with or without the fix.
func handoffTypeRunsCode(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *types.Signature, *types.Interface, *types.TypeParam:
		return true
	case *types.Pointer:
		return handoffTypeRunsCode(u.Elem(), seen)
	case *types.Slice:
		return handoffTypeRunsCode(u.Elem(), seen)
	case *types.Array:
		return handoffTypeRunsCode(u.Elem(), seen)
	case *types.Chan:
		return handoffTypeRunsCode(u.Elem(), seen)
	case *types.Map:
		return handoffTypeRunsCode(u.Key(), seen) || handoffTypeRunsCode(u.Elem(), seen)
	case *types.Struct:
		for field := range u.Fields() {
			if handoffTypeRunsCode(field.Type(), seen) {
				return true
			}
		}
	}
	return false
}
