// Package typeutil provides type-related utilities for GORM analysis.
//
// # Overview
//
// This package provides utilities to:
//   - Check if a type is *gorm.DB
//   - Identify immutable-returning (pure) GORM methods
//
// # Type Detection
//
// The IsGormDB function checks if a type is exactly *gorm.io/gorm.DB.
// It uses exact package path matching to prevent false positives from
// malicious packages like "evil.com/fake-gorm.io/gorm".
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

// IsGormDB checks if the given type is *gorm.DB.
func IsGormDB(t types.Type) bool {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	return isGormDBNamed(ptr.Elem())
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

// finisherMethods are methods that execute queries and interact with the database.
// These are "terminal" operations that typically end a method chain.
var finisherMethods = map[string]struct{}{
	// Query finishers
	"Find":  {},
	"First": {},
	"Last":  {},
	"Take":  {},
	// Aggregate finishers
	"Count": {},
	"Pluck": {},
	"Scan":  {},
	// Mutation finishers
	"Create":  {},
	"Save":    {},
	"Update":  {},
	"Updates": {},
	"Delete":  {},
	// Raw SQL finishers
	"Row":  {},
	"Rows": {},
	"Exec": {},
}

// IsFinisherBuiltin returns true if the method is a finisher that executes queries.
// Finisher methods interact with the database and are typically terminal operations.
func IsFinisherBuiltin(name string) bool {
	_, ok := finisherMethods[name]
	return ok
}
