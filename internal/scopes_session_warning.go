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

// isScopesCallback reports whether fn is a callback passed to the Scopes method,
// via either the variadic-packing path (Store→IndexAddr→Alloc→Slice→Scopes) or a
// direct function/closure argument.
func isScopesCallback(fn *ssa.Function, parent *ssa.Function) bool {
	for _, block := range parent.Blocks {
		for _, instr := range block.Instrs {
			if storePacksFuncIntoScopes(instr, fn) || callPassesFuncToScopes(instr, fn) {
				return true
			}
		}
	}
	return false
}

// storePacksFuncIntoScopes reports whether instr stores fn into a variadic array
// that is then sliced and handed to Scopes.
func storePacksFuncIntoScopes(instr ssa.Instruction, fn *ssa.Function) bool {
	store, ok := instr.(*ssa.Store)
	if !ok || !storeRefersToFunction(store, fn) {
		return false
	}
	indexAddr, ok := store.Addr.(*ssa.IndexAddr)
	if !ok {
		return false
	}
	alloc, ok := indexAddr.X.(*ssa.Alloc)
	if !ok {
		return false
	}
	return allocFlowsToScopes(alloc)
}

// allocFlowsToScopes reports whether a slice of alloc is passed to Scopes.
func allocFlowsToScopes(alloc *ssa.Alloc) bool {
	refs := alloc.Referrers()
	if refs == nil {
		return false
	}
	for _, ref := range *refs {
		if slice, ok := ref.(*ssa.Slice); ok && sliceFlowsToScopes(slice) {
			return true
		}
	}
	return false
}

// sliceFlowsToScopes reports whether slice is an argument to a Scopes call.
func sliceFlowsToScopes(slice *ssa.Slice) bool {
	refs := slice.Referrers()
	if refs == nil {
		return false
	}
	for _, ref := range *refs {
		if call, ok := ref.(*ssa.Call); ok && getMethodName(call) == "Scopes" && isGormDBMethodCall(call) {
			return true
		}
	}
	return false
}

// callPassesFuncToScopes reports whether instr is a Scopes call that receives fn
// directly as an argument (a bare function or a closure).
func callPassesFuncToScopes(instr ssa.Instruction, fn *ssa.Function) bool {
	call, ok := instr.(*ssa.Call)
	if !ok || getMethodName(call) != "Scopes" || !isGormDBMethodCall(call) {
		return false
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
