package directive

import "go/types"

// =============================================================================
// Signature Validation
// =============================================================================

// hasGormDBParameter checks if a function signature has any parameter
// containing *gorm.DB (directly or in struct fields).
func hasGormDBParameter(sig *types.Signature) bool {
	params := sig.Params()
	for i := 0; i < params.Len(); i++ {
		if containsGormDB(params.At(i).Type()) {
			return true
		}
	}
	return false
}

// hasGormDBReturn checks if a function signature has any return value
// containing *gorm.DB (directly or in struct fields).
func hasGormDBReturn(sig *types.Signature) bool {
	results := sig.Results()
	for i := 0; i < results.Len(); i++ {
		if containsGormDB(results.At(i).Type()) {
			return true
		}
	}
	return false
}

// containsGormDB checks if a type contains *gorm.DB anywhere in its structure.
// It recursively checks struct fields, slices, arrays, maps, and channels.
func containsGormDB(t types.Type) bool {
	cache := make(map[types.Type]*cacheEntry)
	return containsGormDBWithCache(t, cache)
}

// cacheEntry tracks the state of type checking to handle cycles.
type cacheEntry struct {
	inProgress bool // Currently being checked (for cycle detection)
	result     bool // Cached result after checking
}

// containsGormDBWithCache performs the actual type checking with cycle detection.
func containsGormDBWithCache(t types.Type, cache map[types.Type]*cacheEntry) bool {
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
	cache[t] = &cacheEntry{inProgress: true}

	// Direct *gorm.DB check
	if isGormDB(t) {
		cache[t] = &cacheEntry{inProgress: false, result: true}
		return true
	}

	// Check underlying type (handles defined types like `type DefinedDB *gorm.DB`)
	underlying := t.Underlying()
	if isGormDB(underlying) {
		cache[t] = &cacheEntry{inProgress: false, result: true}
		return true
	}

	result := false
	switch typ := underlying.(type) {
	case *types.Struct:
		for i := 0; i < typ.NumFields(); i++ {
			if containsGormDBWithCache(typ.Field(i).Type(), cache) {
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
	cache[t] = &cacheEntry{inProgress: false, result: result}
	return result
}

// isGormDB checks if a type is exactly *gorm.DB.
func isGormDB(t types.Type) bool {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Name() == "DB" && obj.Pkg().Path() == "gorm.io/gorm"
}
