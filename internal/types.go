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

// SafeMethods are methods that return a new immutable instance.
// After calling these at the end of a chain, the returned *gorm.DB can be reused.
var SafeMethods = map[string]struct{}{
	"Session":     {},
	"WithContext": {},
}

// DBInitMethods are methods that create a new DB instance.
// These are starting points for chains.
var DBInitMethods = map[string]struct{}{
	"Begin":       {},
	"Transaction": {},
}

// IsSafeMethod returns true if the method name is a safe method.
func IsSafeMethod(name string) bool {
	_, ok := SafeMethods[name]
	return ok
}

// IsDBInitMethod returns true if the method name is a DB init method.
func IsDBInitMethod(name string) bool {
	_, ok := DBInitMethods[name]
	return ok
}

// IsChainMethod returns true if the method is a chain method
// (modifies internal state). This is the default - anything that's
// not a safe method or DB init method is a chain method.
func IsChainMethod(name string) bool {
	return !IsSafeMethod(name) && !IsDBInitMethod(name)
}
