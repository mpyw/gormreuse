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
package typeutil

import (
	"go/types"
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
	// Check for *gorm.DB (most common case)
	if ptr, ok := t.(*types.Pointer); ok {
		return isGormDBNamed(ptr.Elem())
	}
	// Check for gorm.DB (non-pointer, still dangerous due to *Statement field)
	return isGormDBNamed(t)
}

// isGormDBNamed checks if the type is gorm.DB (named type).
func isGormDBNamed(t types.Type) bool {
	named, ok := t.(*types.Named)
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

// =============================================================================
// Immutable Input Methods
// =============================================================================

// ImmutableInputBuiltinDecl describes a builtin GORM method that hands its
// callback a freshly created *gorm.DB.
type ImmutableInputBuiltinDecl struct {
	// CallbackParam is the source-level parameter name (for diagnostics).
	CallbackParam string
	// ParamIdx is the 0-based index of the callback parameter in the
	// method's signature, **excluding the receiver**. Callers that
	// inspect SSA Args must add 1 for static method calls (where Args[0]
	// holds the receiver) and use the value as-is for interface invokes.
	ParamIdx int
}

// immutableInputBuiltins are methods that pass immutable *gorm.DB to their
// callbacks. They are recognised by name *and* by which argument position
// the closure occupies — checking the name alone would mis-classify
// non-callback arguments such as FindInBatches' `dest` and `batchSize`.
//
// All entries are methods on *gorm.DB. Callers must additionally verify the
// receiver type before trusting these constraints.
var immutableInputBuiltins = map[string]ImmutableInputBuiltinDecl{
	// func (db *DB) Transaction(fc func(tx *DB) error, opts ...*sql.TxOptions) error
	"Transaction": {CallbackParam: "fc", ParamIdx: 0},
	// func (db *DB) Connection(fc func(tx *DB) error) error
	"Connection": {CallbackParam: "fc", ParamIdx: 0},
	// func (db *DB) FindInBatches(dest interface{}, batchSize int, fc func(tx *DB, batch int) error) *DB
	"FindInBatches": {CallbackParam: "fc", ParamIdx: 2},
}

// ImmutableInputBuiltin returns the builtin metadata for a method name.
// Callers must additionally verify that the method's receiver is *gorm.DB
// before treating the result as authoritative — the name alone is not safe.
func ImmutableInputBuiltin(name string) (ImmutableInputBuiltinDecl, bool) {
	decl, ok := immutableInputBuiltins[name]
	return decl, ok
}

// IsImmutableInputBuiltin reports whether the method name is one of the
// known builtins. It is a simple presence check; use ImmutableInputBuiltin
// to recover the callback parameter position.
func IsImmutableInputBuiltin(name string) bool {
	_, ok := immutableInputBuiltins[name]
	return ok
}
