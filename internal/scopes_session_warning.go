package internal

import (
	"go/token"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// Scopes Session() Warning (TEMPORARY — GORM bug workaround)
// =============================================================================
//
// GORM has a bug where calling Session()/WithContext()/Debug() inside a
// Scopes callback corrupts the InstanceSet/InstanceGet bookkeeping and
// causes transaction leaks:
//
//   - Issue: https://github.com/go-gorm/gorm/issues/7592
//   - Patch: https://github.com/go-gorm/gorm/pull/7593
//
// **Removal condition**: once the upstream fix ships in a tagged GORM
// release and gormreuse drops support for older versions, delete this
// entire file and the call site in RunSSA. The remaining detection
// (Scopes callback parameter reuse) is handled by Phase 1 alone.
//
// Why this lives in its own file: it is independent of pollution
// tracking, has a known sunset, and would otherwise pollute the
// general orchestration code in analyzer.go.

// scopesWarning represents a warning about Session()-family calls
// inside a Scopes callback.
type scopesWarning struct {
	Pos     token.Pos
	Message string
}

// validateScopesCallback checks whether fn is a Scopes callback and warns
// about Session()/WithContext()/Debug() calls inside it. These three are
// the methods that touch the broken InstanceSet/InstanceGet path.
func validateScopesCallback(fn *ssa.Function) []scopesWarning {
	parent := fn.Parent()
	if parent == nil {
		return nil
	}
	if !isScopesCallback(fn, parent) {
		return nil
	}

	var warnings []scopesWarning
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			// Only flag the GORM bug when these names are actually
			// methods on *gorm.DB; an unrelated package's Session() or
			// Debug() shouldn't trigger the warning.
			if !isGormDBMethodCall(call) {
				continue
			}
			switch getMethodName(call) {
			case "Session":
				warnings = append(warnings, scopesWarning{
					Pos:     call.Pos(),
					Message: "Session() in Scopes callback causes transaction leak (GORM bug)",
				})
			case "WithContext":
				warnings = append(warnings, scopesWarning{
					Pos:     call.Pos(),
					Message: "WithContext() in Scopes callback causes transaction leak (calls Session internally)",
				})
			case "Debug":
				warnings = append(warnings, scopesWarning{
					Pos:     call.Pos(),
					Message: "Debug() in Scopes callback causes transaction leak (calls Session internally)",
				})
			}
		}
	}
	return warnings
}

// storeRefersToFunction reports whether store's value is fn directly or
// a MakeClosure wrapping fn.
func storeRefersToFunction(store *ssa.Store, fn *ssa.Function) bool {
	if directFn, ok := store.Val.(*ssa.Function); ok && directFn == fn {
		return true
	}
	if mc, ok := store.Val.(*ssa.MakeClosure); ok {
		if closureFn, ok := mc.Fn.(*ssa.Function); ok && closureFn == fn {
			return true
		}
	}
	return false
}

// isScopesCallback reports whether fn is a callback passed to the
// Scopes method. It checks both:
//  1. Closures stored via Store→IndexAddr→Alloc→Slice→Scopes (variadic packing).
//  2. Direct function references passed straight as a Scopes argument.
func isScopesCallback(fn *ssa.Function, parent *ssa.Function) bool {
	// Variadic packing path: Store our function into the spread array,
	// then walk forward through Slice → Scopes.
	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok || !storeRefersToFunction(store, fn) {
				continue
			}
			indexAddr, ok := store.Addr.(*ssa.IndexAddr)
			if !ok {
				continue
			}
			alloc, ok := indexAddr.X.(*ssa.Alloc)
			if !ok {
				continue
			}
			allocRefs := alloc.Referrers()
			if allocRefs == nil {
				continue
			}
			for _, ref := range *allocRefs {
				slice, ok := ref.(*ssa.Slice)
				if !ok {
					continue
				}
				sliceRefs := slice.Referrers()
				if sliceRefs == nil {
					continue
				}
				for _, sliceRef := range *sliceRefs {
					if call, ok := sliceRef.(*ssa.Call); ok &&
						getMethodName(call) == "Scopes" && isGormDBMethodCall(call) {
						return true
					}
				}
			}
		}
	}

	// Direct argument path: closure (or function) is passed straight to Scopes.
	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok || getMethodName(call) != "Scopes" || !isGormDBMethodCall(call) {
				continue
			}
			for _, arg := range call.Call.Args {
				if funcVal, ok := arg.(*ssa.Function); ok && funcVal == fn {
					return true
				}
				if mc, ok := arg.(*ssa.MakeClosure); ok {
					if closureFn, ok := mc.Fn.(*ssa.Function); ok && closureFn == fn {
						return true
					}
				}
			}
		}
	}

	return false
}

// getMethodName extracts the method name from a call instruction.
func getMethodName(call *ssa.Call) string {
	if call.Call.IsInvoke() {
		return call.Call.Method.Name()
	}
	if callee := call.Call.StaticCallee(); callee != nil {
		return callee.Name()
	}
	return ""
}

// isGormDBMethodCall reports whether call is dispatching to a method on
// *gorm.DB. We need this whenever we key off a method *name*
// (Session/WithContext/Debug/Scopes) so that an unrelated package's
// identically-named method doesn't trigger GORM-specific diagnostics.
func isGormDBMethodCall(call *ssa.Call) bool {
	if call.Call.IsInvoke() {
		return typeutil.IsGormDB(call.Call.Value.Type())
	}
	callee := call.Call.StaticCallee()
	if callee == nil || callee.Signature == nil || callee.Signature.Recv() == nil {
		return false
	}
	return typeutil.IsGormDB(callee.Signature.Recv().Type())
}
