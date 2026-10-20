package callback

import (
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/origin"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// CollectScopesCallbacks returns the set of functions passed as callbacks to
// gorm's Scopes / Preload across all given functions and their nested closures.
//
// Such callbacks receive a mid-chain (clone==0) *gorm.DB at runtime, so their
// *gorm.DB parameter is a mutable root and reuse inside them interferes (#60).
// This is deliberately narrow — only functions actually handed to Scopes/Preload
// qualify, NOT every func(*gorm.DB) *gorm.DB (that broader treatment is Phase
// 1b, not 1a).
//
// Scopes(funcs ...func(*DB) *DB) and Preload(query, args ...interface{}) are
// variadic, so the callbacks are packed into a varargs array (the last call
// argument is a slice of it); Preload additionally boxes them in interface{}.
//
// vars resolves calls through package-level function variables; it may be
// nil. So do the other collectors.
func CollectScopesCallbacks(funcs []*ssa.Function, vars callsite.FuncVars) map[*ssa.Function]bool {
	return collectCallbacksPassedTo(funcs, vars, "Scopes", "Preload")
}

// CollectScopesOnlyCallbacks is CollectScopesCallbacks without Preload: the
// functions passed to gorm's Scopes, resolved the same way. The temporary
// Session-in-Scopes warning (go-gorm/gorm#7592) uses it, so it covers the same
// callbacks as reuse detection (#196).
func CollectScopesOnlyCallbacks(funcs []*ssa.Function, vars callsite.FuncVars) map[*ssa.Function]bool {
	return collectCallbacksPassedTo(funcs, vars, "Scopes")
}

// CollectImmutableCallbacks returns the set of function literals passed as the
// callback to gorm's Transaction or FindInBatches across all given functions and
// their nested closures.
//
// Each of these methods hands the callback a FRESH, forkable (clone>0) handle
// (Transaction via Begin, FindInBatches via Session(&Session{NewDB: true})), so — unlike an ordinary Phase
// 1b parameter — the callback's *gorm.DB parameter is NOT a mutable root and
// reuse inside the callback is safe (verified against gorm's clone semantics; see
// the epic's pivotal finding). It must be exempted from the mutable-by-default
// treatment (#60 SC103, #62).
func CollectImmutableCallbacks(funcs []*ssa.Function, vars callsite.FuncVars) map[*ssa.Function]bool {
	set := make(map[*ssa.Function]bool)
	addCollectedCallbacks(funcs, vars, nil, set)
	return set
}

// CollectImmutableInputCallbacks adds, to into, every closure passed as the
// callback argument declared by a //gormreuse:immutable-input(name) function.
// Such a callback receives an immutable *gorm.DB (the declaring function promised
// to hand it one), so — like a Transaction callback — its *gorm.DB parameter is
// exempt from the Phase 1b mutable-by-default treatment (#62 case 2.2).
//
// It adds the callbacks of CollectImmutableCallbacks too, so into is meant to
// hold that set already.
func CollectImmutableInputCallbacks(funcs []*ssa.Function, vars callsite.FuncVars, set *directive.ImmutableInputSet, into map[*ssa.Function]bool) {
	if set == nil || into == nil {
		return
	}
	addCollectedCallbacks(funcs, vars, set, into)
}

// CollectImmutableInputProviders maps each function that CollectImmutableInputCallbacks
// adds to the functions that hand its *gorm.DB parameter a value: gorm's
// Transaction or FindInBatches, and the immutable-input functions it is
// passed to, as declared functions (origin.Declared). Its parameter is
// immutable only while every one of them keeps its promise (#314).
func CollectImmutableInputProviders(funcs []*ssa.Function, vars callsite.FuncVars, inputs *directive.ImmutableInputSet) map[*ssa.Function][]*ssa.Function {
	providers := make(map[*ssa.Function][]*ssa.Function)
	h := collectHolders(funcs, vars)
	// Go and defer statements count too: the callee hands the callback the
	// same value however the callee itself is invoked (#256).
	callsite.EachSite(funcs, vars, func(site callsite.Site) {
		args := site.Instr.Common().Args
		for _, idx := range CollectedCallbackArgs(site, inputs) {
			if fn := resolveExactFunc(args[idx], h); fn != nil {
				providers[fn] = append(providers[fn], origin.Declared(site.Declared))
			}
		}
	})
	return providers
}

// CollectedCallbackArgs returns the indices into the instruction's arguments
// (receiver included) of the arguments of site that receive a fresh or
// immutable *gorm.DB as a callback:
//   - for gorm's Transaction or FindInBatches, every argument passed to a
//     parameter of a function type with a *gorm.DB parameter. The callback
//     sits at a different position per method (Transaction: first argument;
//     FindInBatches: third), so the parameters are scanned rather than an
//     index hard-coded.
//   - for a function declaring //gormreuse:immutable-input, the declared
//     callbacks in inputs.
//
// A method value call is the call of its method, and a call through a
// package-level variable is the call of the function it holds (#257).
func CollectedCallbackArgs(site callsite.Site, inputs *directive.ImmutableInputSet) []int {
	if site.Declared == nil {
		return nil
	}
	args := site.Instr.Common().Args
	var idxs []int
	if CollectsFreshCallbacks(site) {
		params := site.Declared.Signature.Params()
		for i := range args {
			if p := site.ArgParam(i); p >= 0 && p < params.Len() {
				if sig, ok := params.At(p).Type().Underlying().(*types.Signature); ok && typeutil.HasGormDBParameter(sig) {
					idxs = append(idxs, i)
				}
			}
		}
		return idxs
	}
	// ParamIdx counts from the first non-receiver parameter, and a method
	// call carries its receiver as the first argument.
	shift := len(args) - len(site.Args)
	for _, cb := range inputs.Callbacks(site.Declared) {
		if idx := cb.ParamIdx + shift; idx >= 0 && idx < len(args) {
			idxs = append(idxs, idx)
		}
	}
	return idxs
}

// CollectsFreshCallbacks reports whether site calls a gorm method that hands a
// fresh (clone>0) *gorm.DB to a callback — Transaction or FindInBatches —
// gated on a gorm.DB receiver so a same-named user method is excluded.
//
// Connection is deliberately absent: it calls fc(db.getInstance()), and
// getInstance yields a clone==0 handle, so its callback's tx accumulates
// conditions like any mid-chain value (#173).
func CollectsFreshCallbacks(site callsite.Site) bool {
	switch site.Method() {
	case "Transaction", "FindInBatches":
		return site.IsGormMethod()
	}
	return false
}

// addCollectedCallbacks adds, to set, the function passed at each
// CollectedCallbackArgs position of every call in funcs.
func addCollectedCallbacks(funcs []*ssa.Function, vars callsite.FuncVars, inputs *directive.ImmutableInputSet, set map[*ssa.Function]bool) {
	for fn := range CollectImmutableInputProviders(funcs, vars, inputs) {
		set[fn] = true
	}
}

// collectCallbacksPassedTo returns every function passed as a variadic
// callback to a call of one of the gorm methods, resolved by resolveMayFuncs.
func collectCallbacksPassedTo(funcs []*ssa.Function, vars callsite.FuncVars, methods ...string) map[*ssa.Function]bool {
	var vals []ssa.Value
	// Go and defer statements count too: the callee hands the callback the
	// same value however the callee itself is invoked (#256).
	callsite.EachSite(funcs, vars, func(site callsite.Site) {
		if !site.IsGormMethod() || !slices.Contains(methods, site.Method()) {
			return
		}
		// The variadic callbacks arrive as a slice in the last argument.
		if args := site.Args; len(args) > 0 {
			vals = append(vals, args[len(args)-1])
		}
	})
	return resolveMayFuncs(funcs, vals)
}
