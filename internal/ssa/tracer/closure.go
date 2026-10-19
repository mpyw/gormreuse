package tracer

import (
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

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
func isClosureResultStored(call *ssa.Call) bool {
	return isClosureResultStoredRecursive(call, make(map[*ssa.Call]bool))
}

func isClosureResultStoredRecursive(call *ssa.Call, visited map[*ssa.Call]bool) bool {
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
			callee := user.Call.StaticCallee()
			if callee == nil {
				continue
			}
			sig := callee.Signature
			if sig == nil || sig.Recv() == nil || !typeutil.IsGormDB(sig.Recv().Type()) {
				continue
			}

			// Check if our result is receiver of this gorm method
			if len(user.Call.Args) > 0 && user.Call.Args[0] == call {
				// Our result is receiver - check if THAT call is stored
				if isClosureResultStoredRecursive(user, visited) {
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
// Unlike containsGormDB in the directive package, this does NOT check:
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
