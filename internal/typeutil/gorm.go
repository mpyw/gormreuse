// Package typeutil provides type-related utilities for GORM analysis.
//
// # Overview
//
// This package provides utilities to:
//   - Check if a type is *gorm.DB or gorm.DB
//   - Identify immutable-returning (pure) GORM methods
//
// # Type Detection
//
// The IsGormDB function checks if a type is exactly *gorm.io/gorm.DB or gorm.io/gorm.DB.
// Both are dangerous because gorm.DB contains *Statement which is shared on copy.
// It uses exact package path matching to prevent false positives from
// malicious packages like "evil.com/fake-gorm.io/gorm".
//
// Note: Nested pointers (**gorm.DB) and interfaces are handled separately:
//   - ClosureCapturesGormDB in tracer package handles **gorm.DB from closure captures
//   - containsGormDB in directive package handles interfaces conservatively
//
// # Pure Method Classification
//
// GORM methods are classified as:
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│  Method Category  │  Examples             │  Returns                    │
//	├─────────────────────────────────────────────────────────────────────────┤
//	│  Pure (clone:1)   │  Session, WithContext │  New immutable *gorm.DB     │
//	│                   │  Debug, Open, Begin   │  (safe to reuse result)     │
//	├─────────────────────────────────────────────────────────────────────────┤
//	│  Non-pure         │  Where, Find, Count   │  Mutable *gorm.DB           │
//	│  (all others)     │  Order, Limit, etc.   │  (shares internal state)    │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// Pure methods return a new *gorm.DB with clone:1, which creates a fresh
// Statement and is safe to reuse. Non-pure methods create shallow clones
// that share the internal Statement.
//
// Two of them are not fully clean (#174); see ImmutableBuiltinEffect:
//   - Session and Debug on a clone==0 receiver return a handle that points at
//     the receiver's Statement, so later changes to the receiver leak into it.
//   - Session(&gorm.Session{Initialized: true}) returns a clone==0 handle.
package typeutil

import (
	"go/constant"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

const (
	gormPkgPath = "gorm.io/gorm"
	gormDBType  = "DB"
)

// =============================================================================
// Type Detection
// =============================================================================

// IsGormDB checks if the given type is *gorm.DB or gorm.DB.
// Both are dangerous because gorm.DB contains *Statement which is shared on copy.
//
// Note: This function does NOT handle nested pointers (**gorm.DB) or interfaces.
// For nested pointers in closure captures, see ClosureCapturesGormDB in tracer package.
// For conservative checks including interfaces, see containsGormDB in directive package.
func IsGormDB(t types.Type) bool {
	// An alias (`type Q = *gorm.DB`) is transparent here: go/types always
	// materializes alias declarations as *types.Alias nodes, so the pointer and
	// named checks below need the aliased type.
	t = types.Unalias(t)
	// Check for *gorm.DB (most common case)
	if ptr, ok := t.(*types.Pointer); ok {
		return isGormDBNamed(ptr.Elem())
	}
	// Check for gorm.DB (non-pointer, still dangerous due to *Statement field)
	return isGormDBNamed(t)
}

// HasGormDBResult reports whether a call result of type t carries a *gorm.DB:
// t is *gorm.DB itself, or a tuple (a multi-value result such as
// (*gorm.DB, error)) with a *gorm.DB element.
func HasGormDBResult(t types.Type) bool {
	tuple, ok := t.(*types.Tuple)
	if !ok {
		return IsGormDB(t)
	}
	for v := range tuple.Variables() {
		if IsGormDB(v.Type()) {
			return true
		}
	}
	return false
}

// IsGormPackage reports whether pkg is exactly the gorm.io/gorm package.
//
// This is used to distinguish genuine gorm builtins (e.g. the package-level
// gorm.Open function) from user-defined functions that merely share a name.
// It uses exact path matching, consistent with isGormDBNamed.
func IsGormPackage(pkg *types.Package) bool {
	return pkg != nil && pkg.Path() == gormPkgPath
}

// isGormDBNamed checks if the type is gorm.DB (named type).
func isGormDBNamed(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	// Use exact match to prevent false positives from packages like "evil.com/fake-gorm.io/gorm"
	return obj.Name() == gormDBType && obj.Pkg().Path() == gormPkgPath
}

// =============================================================================
// Method Classification
// =============================================================================

// immutableReturningMethods are methods/functions that return a new immutable
// *gorm.DB instance (clone: 1). These include:
//   - Safe methods: Session, WithContext, Debug (can be used mid-chain)
//   - Init methods: Open, Begin, Transaction (start new chains)
//
// This map is unexported to prevent external modification.
var immutableReturningMethods = map[string]struct{}{
	// Safe methods - return immutable copy
	"Session":     {},
	"WithContext": {},
	"Debug":       {},
	// Init methods - create new instance
	"Open":        {},
	"Begin":       {},
	"Transaction": {},
}

// IsImmutableReturningBuiltin returns true if the builtin method returns immutable *gorm.DB.
// These methods (Session, WithContext, Debug, Open, Begin, Transaction) return a new
// immutable instance that can be branched freely without pollution.
//
// Note: This is different from user-defined pure functions (//gormreuse:pure),
// which only guarantee no argument pollution - they may return mutable values.
func IsImmutableReturningBuiltin(name string) bool {
	_, ok := immutableReturningMethods[name]
	return ok
}

// BuiltinEffect describes how one call to an immutable-returning builtin
// relates to its receiver and what it returns. Both fields are conservative:
// each is true unless the call is known not to have that effect.
type BuiltinEffect struct {
	// SharesStatement is true when the result points at the receiver's
	// Statement instead of a copy. The result forks a fresh Statement on each
	// use, so using it never changes the receiver. But when the receiver is
	// clone==0, a later in-place change to the receiver leaks into every later
	// use of the result.
	SharesStatement bool
	// ReturnsMutable is true when the result is a clone==0 handle.
	ReturnsMutable bool
}

// ImmutableBuiltinEffect reports the effect of a call to the builtin named
// name. sessionConfig is the *gorm.Session argument of a Session call, and is
// ignored for other names. The caller must make sure the callee is the real
// gorm builtin.
//
// GORM v1.31 behaves as follows:
//   - Debug calls Session with only a Logger, so it shares.
//   - WithContext calls Session with a Context, so it does not share.
//   - Session clones the Statement when Context, PrepareStmt or SkipHooks is
//     set. With NewDB, each use starts a new Statement. Otherwise it shares.
//     With Initialized it returns tx.getInstance(): a clone==0 handle that
//     has its own copy of the Statement.
//
// When sessionConfig is not a &gorm.Session{...} literal that this function
// can read, both effects are assumed. A Context field set to anything other
// than the nil constant counts as set, as for WithContext(ctx).
func ImmutableBuiltinEffect(name string, sessionConfig ssa.Value) BuiltinEffect {
	switch name {
	case "Debug":
		return BuiltinEffect{SharesStatement: true}
	case "Session":
		fields, ok := readSessionLiteral(sessionConfig)
		if !ok {
			return BuiltinEffect{SharesStatement: true, ReturnsMutable: true}
		}
		isolated := fields["Context"] == sessionFieldTrue ||
			fields["PrepareStmt"] == sessionFieldTrue ||
			fields["SkipHooks"] == sessionFieldTrue ||
			fields["NewDB"] == sessionFieldTrue
		initialized := fields["Initialized"]
		return BuiltinEffect{
			SharesStatement: !isolated && initialized != sessionFieldTrue,
			ReturnsMutable:  initialized != sessionFieldFalse,
		}
	default:
		return BuiltinEffect{}
	}
}

// sessionFieldState is what readSessionLiteral knows about one field. The
// zero value is false, which is also the value of a field the literal omits.
type sessionFieldState uint8

const (
	sessionFieldFalse sessionFieldState = iota
	sessionFieldTrue
	sessionFieldUnknown
)

// readSessionLiteral reads the fields of a &gorm.Session{...} value. In SSA,
// such a literal is an Alloc with one FieldAddr and Store per set field. ok is
// false when v is anything else, or when the Alloc is used in some way other
// than a field store or a call to a Session method, since that use could
// change it. A store in another block than the Alloc, such as one under an
// if, makes its field unknown.
func readSessionLiteral(v ssa.Value) (fields map[string]sessionFieldState, ok bool) {
	alloc, isAlloc := v.(*ssa.Alloc)
	if !isAlloc {
		return nil, false
	}
	ptr, isPtr := alloc.Type().Underlying().(*types.Pointer)
	if !isPtr {
		return nil, false
	}
	st, isStruct := ptr.Elem().Underlying().(*types.Struct)
	if !isStruct {
		return nil, false
	}

	fields = map[string]sessionFieldState{}
	for _, ref := range *alloc.Referrers() {
		switch r := ref.(type) {
		case *ssa.FieldAddr:
			name := st.Field(r.Field).Name()
			for _, fieldRef := range *r.Referrers() {
				store, isStore := fieldRef.(*ssa.Store)
				if !isStore || store.Addr != r {
					return nil, false
				}
				state := sessionFieldUnknown
				if store.Block() == alloc.Block() {
					state = sessionStoredState(store.Val)
				}
				if prev, seen := fields[name]; seen && prev != state {
					state = sessionFieldUnknown
				}
				fields[name] = state
			}
		case *ssa.Call:
			callee := r.Call.StaticCallee()
			if callee == nil || (callee.Name() != "Session" && callee.Name() != "Session$bound") {
				return nil, false
			}
		case *ssa.DebugRef:
		default:
			return nil, false
		}
	}
	return fields, true
}

// sessionStoredState reads a value stored into a gorm.Session field. A
// constant is read as a bool, where the nil or zero constant is false. Any
// other value is unknown, except for an interface (the Context field), which
// counts as set.
func sessionStoredState(v ssa.Value) sessionFieldState {
	c, isConst := v.(*ssa.Const)
	if !isConst {
		if types.IsInterface(v.Type()) {
			return sessionFieldTrue
		}
		return sessionFieldUnknown
	}
	if c.Value == nil {
		return sessionFieldFalse
	}
	if c.Value.Kind() == constant.Bool && constant.BoolVal(c.Value) {
		return sessionFieldTrue
	}
	return sessionFieldFalse
}
