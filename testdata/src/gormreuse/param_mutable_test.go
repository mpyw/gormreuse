package internal

import "gorm.io/gorm"

// =============================================================================
// Phase 1: Parameter Reuse Detection
// After Phase 1, parameters are treated as mutable (not immutable)
// =============================================================================

// ===== 1.1 Basic parameter reuse =====

// parameterReuse demonstrates basic parameter reuse detection.
// Parameters are now treated as mutable, so using the parameter twice is a violation.
func parameterReuse(db *gorm.DB) {
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ===== 1.2 Parameter with Session (correct pattern) =====

// parameterWithSessionCorrect demonstrates safe parameter usage with Session.
func parameterWithSessionCorrect(db *gorm.DB) {
	db = db.Session(&gorm.Session{}) // Make parameter immutable
	db.Find(nil)
	db.Count(nil) // OK - Session creates isolation
}

// ===== 1.3 Passing parameter to non-pure function =====

// nonPureWrapper is a NON-pure function that pollutes db argument.
func nonPureWrapper(db *gorm.DB) *gorm.DB {
	return db.Where("wrapped")
}

// passingParamToNonPure demonstrates that passing parameter to non-pure function pollutes it.
func passingParamToNonPure(db *gorm.DB) {
	wrapped := nonPureWrapper(db) // This pollutes db
	wrapped.Find(nil)
	db.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ===== 1.4 Passing parameter to pure function =====

// pureWrapper is a pure function that doesn't pollute its argument.
//
//gormreuse:pure
func pureWrapper(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("wrapped")
}

// passingParamToPure demonstrates that passing parameter to pure function is safe.
func passingParamToPure(db *gorm.DB) {
	pureWrapper(db) // Pure doesn't pollute db
	pureWrapper(db) // Still OK
	db.Find(nil)    // OK - db is not polluted by pure functions
}

// ===== 1.5 Mutable reuse (unchanged behavior) =====

// mutableReuse demonstrates that mutable reuse detection still works.
func mutableReuse(db *gorm.DB) {
	q := db.Where("x") // q is mutable
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ===== 1.6 Pure function reusing parameter (THE KEY FIX) =====

// badPure is a pure function that INCORRECTLY reuses its parameter.
// Phase 1 enables detection of this violation.
//
//gormreuse:pure
func badPure(db *gorm.DB) *gorm.DB {
	db.Find(nil)         // want `pure function pollutes \*gorm\.DB argument by calling Find`
	return db.Where("x") // want `pure function pollutes \*gorm\.DB argument by calling Where` `\*gorm\.DB instance reused after chain method`
}

// ===== 1.7 Correct pure function =====

// correctPure is a pure function that correctly uses Session.
//
//gormreuse:pure
func correctPure(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("x") // OK - Session first
}

// ===== 1.8 Pure function reusing mutable (unchanged) =====

// pureWithMutable is a pure function that reuses a mutable value.
//
//gormreuse:pure
func pureWithMutable(db *gorm.DB) *gorm.DB {
	q := db.Where("base")        // want `pure function pollutes \*gorm\.DB argument by calling Where`
	q.Find(nil)                  // want `pure function pollutes \*gorm\.DB argument by calling Find`
	return q.Where("x")          // want `pure function pollutes \*gorm\.DB argument by calling Where` `\*gorm\.DB instance reused after chain method`
}

// ===== 1.9-1.12 Callback reuse auto-detection =====

// These test that callback parameter reuse is automatically detected.
// Scopes and Preload callbacks receive mutable *gorm.DB.

// scopesCallbackReuse demonstrates callback reuse in Scopes is detected.
func scopesCallbackReuse(db *gorm.DB) {
	db = db.Session(&gorm.Session{}) // Make parameter immutable
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		q.Find(nil)
		return q.Where("x") // want `\*gorm\.DB instance reused after chain method`
	})
}

// scopesCallbackCorrect demonstrates correct Scopes callback usage.
func scopesCallbackCorrect(db *gorm.DB) {
	db = db.Session(&gorm.Session{}) // Make parameter immutable
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Where("active = ?", true) // OK - single use
	})
}

// preloadCallbackReuse demonstrates callback reuse in Preload is detected.
func preloadCallbackReuse(db *gorm.DB) {
	db = db.Session(&gorm.Session{}) // Make parameter immutable
	db.Preload("Orders", func(q *gorm.DB) *gorm.DB {
		q.Find(nil)
		return q.Where("x") // want `\*gorm\.DB instance reused after chain method`
	})
}

// preloadCallbackCorrect demonstrates correct Preload callback usage.
func preloadCallbackCorrect(db *gorm.DB) {
	db = db.Session(&gorm.Session{}) // Make parameter immutable
	db.Preload("Orders", func(q *gorm.DB) *gorm.DB {
		return q.Order("created_at DESC") // OK - single use
	})
}
