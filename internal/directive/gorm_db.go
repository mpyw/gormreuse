package directive

import (
	"go/types"

	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// *gorm.DB Detection Predicates
// =============================================================================

// HasGormDBParameter reports whether a function signature has any parameter
// containing *gorm.DB. It is the same predicate that decides whether a
// //gormreuse:immutable-param directive has a valid signature, exported so
// callers (e.g. redundant-directive detection) can distinguish a signature-valid
// annotation from a signature-invalid one.
func HasGormDBParameter(sig *types.Signature) bool { return hasGormDBParameter(sig) }

// hasGormDBParameter checks if a function signature has any parameter
// containing *gorm.DB (directly or in struct fields).
//
//declscope:package // immutable_input.go and funcset.go both gate on the signature
func hasGormDBParameter(sig *types.Signature) bool {
	params := sig.Params()
	for v := range params.Variables() {
		if containsGormDB(v.Type()) {
			return true
		}
	}
	return false
}

// hasGormDBReturn checks if a function signature has any return value
// containing *gorm.DB (directly or in struct fields).
//
//declscope:package // funcset.go gates on the signature too
func hasGormDBReturn(sig *types.Signature) bool {
	results := sig.Results()
	for v := range results.Variables() {
		if containsGormDB(v.Type()) {
			return true
		}
	}
	return false
}

// containsGormDB checks if a type contains *gorm.DB anywhere in its structure.
// It recursively checks struct fields, slices, arrays, maps, and channels.
//
//declscope:package // directive_test.go exercises it directly
func containsGormDB(t types.Type) bool {
	cache := make(map[types.Type]*gormDBCacheEntry)
	return containsGormDBWithCache(t, cache)
}

// gormDBCacheEntry tracks the state of type checking to handle cycles.
type gormDBCacheEntry struct {
	inProgress bool // Currently being checked (for cycle detection)
	result     bool // Cached result after checking
}

// containsGormDBWithCache performs the actual type checking with cycle detection.
func containsGormDBWithCache(t types.Type, cache map[types.Type]*gormDBCacheEntry) bool {
	if t == nil {
		return false
	}

	// Check cache
	if entry, ok := cache[t]; ok {
		if entry.inProgress {
			// Currently checking this type - cycle detected, assume false
			return false
		}
		// Return cached result
		return entry.result
	}

	// Mark as in progress for cycle detection
	cache[t] = &gormDBCacheEntry{inProgress: true}

	// Direct *gorm.DB check
	if typeutil.IsGormDB(t) {
		cache[t] = &gormDBCacheEntry{inProgress: false, result: true}
		return true
	}

	// Check underlying type (handles defined types like `type DefinedDB *gorm.DB`)
	underlying := t.Underlying()
	if typeutil.IsGormDB(underlying) {
		cache[t] = &gormDBCacheEntry{inProgress: false, result: true}
		return true
	}

	result := false
	switch typ := underlying.(type) {
	case *types.Interface:
		// An interface could hold *gorm.DB at runtime, but this predicate only
		// drives unused-directive detection, where we require the signature to
		// name a CONCRETE *gorm.DB before treating the directive as satisfied.
		// The pure/immutable-return analysis never tracks values through an
		// interface-typed parameter/return anyway (it keys on the concrete
		// *gorm.DB type), so a purely interface-typed signature genuinely does
		// nothing — reporting it as unused is the correct, actionable signal
		// (issue #72 gap4). NOTE: containsGormDB is used ONLY for unused
		// detection; the SSA tracer uses its own containsGormDBThroughPointers.
		result = false
	case *types.Struct:
		for field := range typ.Fields() {
			if containsGormDBWithCache(field.Type(), cache) {
				result = true
				break
			}
		}
	case *types.Pointer:
		result = containsGormDBWithCache(typ.Elem(), cache)
	case *types.Slice:
		result = containsGormDBWithCache(typ.Elem(), cache)
	case *types.Array:
		result = containsGormDBWithCache(typ.Elem(), cache)
	case *types.Map:
		result = containsGormDBWithCache(typ.Key(), cache) || containsGormDBWithCache(typ.Elem(), cache)
	case *types.Chan:
		result = containsGormDBWithCache(typ.Elem(), cache)
	}

	// Cache the result
	cache[t] = &gormDBCacheEntry{inProgress: false, result: result}
	return result
}
