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
//	│  Pure (clone>0)   │  Session, WithContext │  New immutable *gorm.DB     │
//	│                   │  Debug, Open, Begin   │  (safe to reuse result)     │
//	│                   │  Transaction          │  error; reads the receiver  │
//	├─────────────────────────────────────────────────────────────────────────┤
//	│  Non-pure         │  Where, Find, Count   │  Mutable *gorm.DB           │
//	│  (all others)     │  Order, Limit, etc.   │  (shares internal state)    │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// Pure methods return a new *gorm.DB with clone>0 (1 for Open and
// Session{NewDB: true}, else 2), which forks a fresh Statement on its next
// chain call and is safe to reuse. A call to one reads its receiver without
// branching it. Transaction returns an error, so only that last part applies
// to it. Non-pure methods return a clone==0 handle that shares the
// receiver's Statement.
//
// Two of them are not fully clean (#174); see callsite.SessionEffect:
//   - Session and Debug on a clone==0 receiver return a handle that points at
//     the receiver's Statement, so later changes to the receiver leak into it.
//   - Session(&gorm.Session{Initialized: true}) returns a clone==0 handle.
package typeutil

import (
	"go/types"
	"slices"
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

// ReachesGorm reports whether pkg is gorm.io/gorm or imports it, directly or
// through other packages. Only then can a value in pkg hold a *gorm.DB: its
// type would name gorm.DB, so the package that declares that type is among
// the imports that the type information of pkg records.
func ReachesGorm(pkg *types.Package) bool {
	seen := make(map[*types.Package]bool)
	var reaches func(p *types.Package) bool
	reaches = func(p *types.Package) bool {
		if seen[p] {
			return false
		}
		seen[p] = true
		return IsGormPackage(p) || slices.ContainsFunc(p.Imports(), reaches)
	}
	return pkg != nil && reaches(pkg)
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

// immutableReturningMethods are methods/functions whose call reads its
// receiver without branching it. All but Transaction return a new immutable
// *gorm.DB instance (clone > 0):
//   - Safe methods: Session, WithContext, Debug (can be used mid-chain)
//   - Init methods: Open, Begin (start new chains)
//   - Transaction returns an error. It runs its callback on a Begin result,
//     and the callback's fresh tx is handled by CollectImmutableCallbacks.
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

// IsImmutableReturningBuiltin reports whether name is one of
// immutableReturningMethods: Session, WithContext, Debug, Open and Begin return
// a new immutable instance that can be branched freely, and Transaction
// returns an error. A call to any of them reads its receiver without
// branching it.
//
// Note: This is different from user-defined pure functions (//gormreuse:pure),
// which only guarantee no argument pollution - they may return mutable values.
func IsImmutableReturningBuiltin(name string) bool {
	_, ok := immutableReturningMethods[name]
	return ok
}
