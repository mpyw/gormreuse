package internal

import (
	"go/ast"
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// reportImmutableReturnViolations enforces the body-side immutable-return
// contract: a function marked //gormreuse:immutable-return must actually return
// an immutable *gorm.DB. Otherwise the tracer trusts the directive
// (returnsImmutable) and silently allows unsafe reuse of the actually-mutable
// return value at every call site. It returns the functions that break the contract.
//
//declscope:shared // run.go reports the immutable-return functions that break their contract
func reportImmutableReturnViolations(
	funcs []*ssa.Function,
	immutableReturnFuncs *directive.DirectiveFuncSet,
	rt *tracer.RootTracer,
	skip func(*ssa.Function) bool,
	report func(token.Pos, string),
	guard *panicGuard,
) map[*ssa.Function]bool {
	failed := make(map[*ssa.Function]bool)
	validator := purity.NewImmutableReturnValidator(funcs, immutableReturnFuncs, rt)
	for _, fn := range funcs {
		if skip(fn) {
			continue
		}
		// A check that panicked proves nothing, so the function is not
		// trusted (#315).
		if !guard.run("immutable-return", fn, func() {
			for _, v := range validator.Validate(fn) {
				failed[fn] = true
				report(v.Pos, v.Message)
			}
		}) {
			failed[fn] = true
		}
	}
	return failed
}

// inferImmutableReturns adds to set the functions of funcs that have no
// immutable-return directive but whose body proves that every *gorm.DB they
// return is immutable (#261). A function of a skipped file is not inferred,
// since its body is not analyzed.
//
// Here a function marked immutable-return counts as immutable even when its
// body breaks the contract, as everywhere else in the package: that body is
// reported once, at its declaration. distrustContracts keeps such trust out
// of the facts.
//
//declscope:shared // run.go infers the immutable-return functions
func inferImmutableReturns(funcs []*ssa.Function, set *directive.DirectiveFuncSet, funcVars callsite.FuncVars, skip func(*ssa.Function) bool) {
	var candidates []*ssa.Function
	for _, fn := range funcs {
		if !skip(fn) && !set.Contains(fn) && immutableReturnCandidate(fn) {
			candidates = append(candidates, fn)
		}
	}
	builtins := tracer.NewRootTracer(nil, nil, nil, nil, nil, nil, funcVars)
	assigned := purity.NewImmutableReturnValidator(funcs, set, builtins)
	inferred := purity.InferImmutableReturns(candidates, set.Contains, set.ParamReads, builtins, assigned.AssignedMutable)
	for _, fn := range candidates {
		if reads, ok := inferred[fn]; ok {
			set.InferParamReads(fn, reads)
		}
	}
}

// immutableReturnCandidate reports whether fn has a *gorm.DB result, or a
// struct result with *gorm.DB fields that callers read as its FieldRoots
// (#342), which inference needs.
func immutableReturnCandidate(fn *ssa.Function) bool {
	for v := range fn.Signature.Results().Variables() {
		if typeutil.IsGormDB(v.Type()) || len(purity.ImmutableReturnFieldPaths(v.Type())) > 0 {
			return true
		}
	}
	return false
}

// computeNeedsImmutableParam returns the //gormreuse:immutable-param functions
// that genuinely rely on immutability: analyzed with their parameters treated as
// mutable (a counterfactual with immutableParamFuncs = nil), they produce at
// least one violation rooted at one of their own parameters — i.e. they branch a
// parameter, so the directive suppresses a real diagnostic.
//
// The result classifies every valid immutable-param function:
//   - in the set   → the directive is necessary; a caller passing a mutable value
//     violates the contract (stage 2b caller-side check consumes this).
//   - not in the set → the directive suppresses nothing; it is redundant
//     (reportRedundantImmutableParam consumes the complement).
//
// Signature-invalid directives (no *gorm.DB parameter) are excluded — they are
// handled by the existing unused-directive path. Functions also marked
// //gormreuse:pure are excluded because a valid pure function cannot branch its
// parameter (branching is pollution the pure contract forbids), so they can never
// need — nor, for redundancy purposes, be flagged for — immutability. One whose
// pure check failed is not excluded: it may branch its parameter (#348).
//
// Known limitation: a function that ONLY forwards its parameter once to another
// immutable-param function (never using it a second time) is not detected as
// needing immutability — the counterfactual sees a single use, not a reuse — so
// it is reported redundant even though removing the directive would surface a 2b
// contract violation at the forwarding call. This is an uncommon delegation
// pattern; suppress with //gormreuse:ignore if intended.
//
//declscope:shared // run.go finds the parameters that need immutable-param
func computeNeedsImmutableParam(
	funcs []*ssa.Function,
	immutableParamFuncs, pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet,
	pureVerdicts *tracer.PureVerdicts,
	scopesCallbacks, immutableCallbacks map[*ssa.Function]bool,
	funcVars callsite.FuncVars,
	skip func(*ssa.Function) bool,
	guard *panicGuard,
	cfgs *cfg.Analyzer,
) map[*ssa.Function]bool {
	needs := make(map[*ssa.Function]bool)
	if immutableParamFuncs == nil {
		return needs
	}
	// The counterfactual treats every parameter as mutable, so it has a tracer
	// of its own, shared by the functions it checks.
	counterfactual := tracer.NewRootTracer(pureFuncs, immutableReturnFuncs, nil, pureVerdicts, scopesCallbacks, immutableCallbacks, funcVars)
	for _, fn := range funcs {
		// Every caller of an inferred function passes immutable values, so
		// its callers need no check (#349).
		if skip(fn) || !immutableParamFuncs.Contains(fn) || immutableParamFuncs.Inferred(fn) {
			continue
		}
		if fn.Signature == nil || !typeutil.HasGormDBParameter(fn.Signature) {
			continue
		}
		// A pure function that broke its contract may branch its parameter,
		// so it is checked as any other (#348).
		if pureFuncs != nil && pureFuncs.Contains(fn) && !pureVerdicts.HasFailed(fn) {
			continue
		}
		// A function whose analysis panicked keeps its directive: reporting
		// it redundant would delete a directive it may need (#315).
		if !guard.run("immutable-param", fn, func() {
			// Counterfactual: analyze fn with its parameters treated as mutable.
			cf := ssautil.NewAnalyzer(fn, counterfactual, nil, cfgs)
			if slices.ContainsFunc(cf.Analyze(), func(v pollution.Violation) bool {
				p, ok := v.Root.(*ssa.Parameter)
				return ok && p.Parent() == fn
			}) {
				needs[fn] = true
			}
		}) {
			needs[fn] = true
		}
	}
	return needs
}

// reportRedundantImmutableParam flags //gormreuse:immutable-param directives that
// are signature-valid (the function has a *gorm.DB parameter) yet have no effect:
// even if the parameter were treated as mutable it would never be reused, so the
// directive suppresses nothing. These are exactly the valid, non-pure directives
// NOT in the needsImmutableParam set (see computeNeedsImmutableParam).
//
// Scope note: Phase 1b stage 2a/2b makes immutable-param a callee-side contract
// plus a caller-side check keyed on needsImmutableParam. A redundant directive is
// (by construction) not in that set, so it triggers no caller-side obligation
// either — removing it changes no diagnostic, which is what "redundant" means.
//
// Signature-invalid directives (no *gorm.DB parameter) are reported as unused by
// reportUnusedDirectiveFuncs, not here; the same guards used when building
// needsImmutableParam keep the two paths from overlapping.
//
// The fix deletes immutable-param from the function's directive comments
// (#248). A comment before an assignment of several closures applies to each
// of them, so it is edited only when no other closure it covers keeps the
// directive.
//
//declscope:shared // run.go reports the immutable-param directives that change nothing
func reportRedundantImmutableParam(
	funcs []*ssa.Function,
	immutableParamFuncs, pureFuncs *directive.DirectiveFuncSet,
	needsImmutableParam map[*ssa.Function]bool,
	skip func(*ssa.Function) bool,
	report reportRemovalFunc,
) {
	if immutableParamFuncs == nil {
		return
	}
	var redundant []*ssa.Function
	kept := make(map[token.Pos]bool) // Comments that a function not reported keeps
	for _, fn := range funcs {
		// An inferred function has no directive to report (#349).
		if skip(fn) || !immutableParamFuncs.Contains(fn) || immutableParamFuncs.Inferred(fn) {
			continue
		}
		if fn.Signature == nil || !typeutil.HasGormDBParameter(fn.Signature) {
			continue // signature-invalid: handled by reportUnusedDirectiveFuncs
		}
		// pure ⇒ param never branched ⇒ redundant by construction (not flagged)
		if pureFuncs != nil && pureFuncs.Contains(fn) || needsImmutableParam[fn] {
			for _, pos := range immutableParamFuncs.CommentsOf(fn) {
				kept[pos] = true
			}
			continue
		}
		redundant = append(redundant, fn)
	}
	for _, fn := range redundant {
		var targets []directive.RemovalTarget
		for _, pos := range immutableParamFuncs.CommentsOf(fn) {
			if kept[pos] {
				targets = nil
				break
			}
			targets = append(targets, directive.RemovalTarget{Comment: pos, Part: "immutable-param"})
		}
		report(fn.Pos(), "redundant gormreuse:immutable-param directive: no *gorm.DB parameter is reused", targets...)
	}
}

// reportImmutableParamDiscards enforces a body-side rule of
// //gormreuse:immutable-param (#346): the function's callers pass an
// isolated *gorm.DB, so a statement that discards a non-finisher chain on a
// parameter, such as `tx.Where("t = 1")`, changes a copy and has no effect.
// Without the directive the same statement would change the caller's value
// in place, so it is a sign that the function relies on that. It returns the
// functions that have such a statement. The fix that isolates an argument to
// one of them is withheld, since it would drop the change. A pure function
// is left to the pure check, which reports such a statement as pollution.
//
//declscope:shared // run.go reports them, and the fix generator withholds an isolation for them
func reportImmutableParamDiscards(
	funcs []*ssa.Function,
	immutableParamFuncs, pureFuncs *directive.DirectiveFuncSet,
	skip func(*ssa.Function) bool,
	report func(token.Pos, string),
	guard *panicGuard,
) map[*ssa.Function]bool {
	discards := make(map[*ssa.Function]bool)
	if immutableParamFuncs == nil {
		return discards
	}
	for _, fn := range funcs {
		if skip(fn) || !immutableParamFuncs.Contains(fn) || (pureFuncs != nil && pureFuncs.Contains(fn)) {
			continue
		}
		// A function whose check panicked counts as one with a discard, so
		// no fix isolates an argument to it.
		if !guard.run("immutable-param", fn, func() {
			var stmts map[token.Pos]*ast.ExprStmt
			for _, p := range fn.Params {
				if !typeutil.IsGormDB(p.Type()) {
					continue
				}
				for _, ref := range *p.Referrers() {
					first, ok := ref.(*ssa.Call)
					if !ok {
						continue
					}
					last, ok := immutableParamDiscardedChain(first, p)
					if !ok {
						continue
					}
					if stmts == nil {
						stmts = immutableParamExprStmts(fn.Syntax())
					}
					// The whole chain is one expression statement, so no
					// variable holds a part of it.
					if stmt := stmts[last.Pos()]; stmt == nil || first.Pos() < stmt.Pos() {
						continue
					}
					discards[fn] = true
					report(first.Pos(), "immutable-param declared but the result of "+p.Name()+"."+first.Call.StaticCallee().Name()+" is discarded: "+p.Name()+" is isolated, so the call has no effect; assign the result or remove the call")
				}
			}
		}) {
			discards[fn] = true
		}
	}
	return discards
}

// immutableParamDiscardedChain reports whether call is the first call of a chain of
// *gorm.DB methods on v whose result nothing reads, as in v.Where("a") or
// v.Where("a").Limit(1), and returns the chain's last call. Each call of the
// chain is a *gorm.DB method that returns a *gorm.DB and is neither a
// finisher nor a Session-family or state-only method (see
// typeutil.IsImmutableReturningBuiltin).
//
//declscope:shared // infer.go refuses to infer immutable-param for a function with such a chain
func immutableParamDiscardedChain(call *ssa.Call, v ssa.Value) (*ssa.Call, bool) {
	for recv := v; ; {
		if !immutableParamChainCall(call, recv) {
			return nil, false
		}
		var next *ssa.Call
		for _, ref := range *call.Referrers() {
			switch ref := ref.(type) {
			case *ssa.DebugRef:
			case *ssa.Call:
				if next != nil {
					return nil, false
				}
				next = ref
			default:
				return nil, false
			}
		}
		if next == nil {
			return call, true
		}
		recv, call = call, next
	}
}

// immutableParamChainCall reports whether call calls, with receiver recv, a *gorm.DB
// method that returns a *gorm.DB and only adds to the receiver's Statement.
func immutableParamChainCall(call *ssa.Call, recv ssa.Value) bool {
	callee := call.Call.StaticCallee()
	if callee == nil || callee.Signature.Recv() == nil || !typeutil.IsGormDB(callee.Signature.Recv().Type()) {
		return false
	}
	args := call.Call.Args
	if len(args) == 0 || args[0] != recv || slices.Contains(args[1:], recv) || !typeutil.IsGormDB(call.Type()) {
		return false
	}
	name := callee.Name()
	return !typeutil.IsFinisher(name) && !typeutil.IsImmutableReturningBuiltin(name)
}

// immutableParamExprStmts returns the expression statements in syntax whose expression
// is a call, by the position of the call's left parenthesis, which is the
// position of its SSA call.
func immutableParamExprStmts(syntax ast.Node) map[token.Pos]*ast.ExprStmt {
	stmts := make(map[token.Pos]*ast.ExprStmt)
	if syntax == nil {
		return stmts
	}
	ast.Inspect(syntax, func(n ast.Node) bool {
		if stmt, ok := n.(*ast.ExprStmt); ok {
			if call, ok := ast.Unparen(stmt.X).(*ast.CallExpr); ok {
				stmts[call.Lparen] = stmt
			}
		}
		return true
	})
	return stmts
}
