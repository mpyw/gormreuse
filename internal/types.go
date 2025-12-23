package internal

import (
	"go/types"
	"strings"
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
	return obj.Name() == gormDBType && strings.HasSuffix(obj.Pkg().Path(), gormPkgPath)
}

// =============================================================================
// Method Classification
// =============================================================================

// ImmutableReturningMethods are methods/functions that return a new immutable
// *gorm.DB instance (clone: 1). These include:
//   - Safe methods: Session, WithContext, Debug (can be used mid-chain)
//   - Init methods: Open, Begin, Transaction (start new chains)
var ImmutableReturningMethods = map[string]struct{}{
	// Safe methods - return immutable copy
	"Session":     {},
	"WithContext": {},
	"Debug":       {},
	// Init methods - create new instance
	"Open":        {},
	"Begin":       {},
	"Transaction": {},
}

// ReturnsImmutable returns true if the method/function returns an immutable *gorm.DB.
func ReturnsImmutable(name string) bool {
	_, ok := ImmutableReturningMethods[name]
	return ok
}
