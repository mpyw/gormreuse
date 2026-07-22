package purity

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// ValidateImmutableReturn enforces the body-side contract of
// //gormreuse:immutable-return: the function must actually return an immutable
// *gorm.DB — a value callers can reuse (branch) freely, exactly like a builtin
// Session()/WithContext() result.
//
// It reports only the case it can prove: a return value whose mutable root is a
// gorm chain-method call (Where, Order, …). That is a definitively mutable
// clone==0 handle — e.g. db.Where(...), or Session().Where(...) whose trailing
// chain method re-forks a fresh clone==0 Statement (see the e2e
// TestSessionInMiddle). The tracer otherwise trusts the directive
// (returnsImmutable) and silently allows unsafe reuse of the return value at
// every call site, so the mismatch is reported at the declaration instead of
// being tolerated.
//
// It deliberately does NOT flag roots the tracer treats as mutable only
// conservatively — a bare *gorm.DB parameter (the caller may have isolated it)
// or a call into a user function/closure the tracer cannot see through (e.g. an
// unmarked closure that itself returns Session()). The directive exists to let
// callers assert immutability the analyzer cannot prove; rejecting those would
// turn the escape hatch into a false positive.
//
// rt must be a tracer configured with the pass's full context so FindAllMutableRoots
// classifies immutable sources (including other immutable-return functions and
// immutable-param'd parameters) correctly.
func ValidateImmutableReturn(fn *ssa.Function, set *directive.DirectiveFuncSet, rt *tracer.RootTracer) []Violation {
	if fn == nil || set == nil || rt == nil || !set.Contains(fn) {
		return nil
	}

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}
			for _, res := range ret.Results {
				// Only *gorm.DB results carry the contract. A vacuous directive
				// (no *gorm.DB in the return, e.g. interface{}) governs nothing
				// and is handled by the unused-directive path, not here.
				if !typeutil.IsGormDB(res.Type()) {
					continue
				}
				for _, root := range rt.FindAllMutableRoots(res, nil) {
					if !isGormChainCall(root) {
						continue // not a provably-mutable root
					}
					// One diagnostic per function: the directive, not each
					// return, is what is wrong. Report at the declaration.
					return []Violation{{
						Pos:     fn.Pos(),
						Message: "immutable-return declared but function returns mutable *gorm.DB",
					}}
				}
			}
		}
	}
	return nil
}

// isGormChainCall reports whether v is the result of a gorm chain method call
// (a method on *gorm.DB that is not an immutable-returning builtin). Such a
// result is a definitively mutable clone==0 handle; every other mutable root the
// tracer produces (parameters, user-function/closure calls) is only a
// conservative guess and must not drive an immutable-return contract violation.
func isGormChainCall(v ssa.Value) bool {
	call, ok := v.(*ssa.Call)
	if !ok {
		return false
	}
	callee := call.Call.StaticCallee()
	if callee == nil || callee.Signature == nil || callee.Signature.Recv() == nil {
		return false
	}
	if !typeutil.IsGormDB(callee.Signature.Recv().Type()) {
		return false
	}
	return !typeutil.IsImmutableReturningBuiltin(callee.Name())
}
