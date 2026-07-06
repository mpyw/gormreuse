package internal

import "gorm.io/gorm"

// =============================================================================
// Phase 1b stage 2b: caller-side //gormreuse:immutable-param contract
//
// A function marked immutable-param that BRANCHES its parameter relies on the
// caller passing an isolated (clone>0) value. Passing a mutable (clone==0) value
// is reported at the call site.
// =============================================================================

// applyTwoFilters relies on immutability: it branches its parameter twice, so a
// caller must pass an isolated value.
//
//gormreuse:immutable-param
func applyTwoFilters(db *gorm.DB) {
	db.Where("a").Find(nil)
	db.Where("b").Find(nil)
}

// ===== SHOULD REPORT =====

// contractPassMutable passes a mutable (mid-chain) value — a contract violation.
func contractPassMutable(root *gorm.DB) {
	q := root.Session(&gorm.Session{}).Where("x") // mutable: chain after Session (clone==0)
	applyTwoFilters(q)                             // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of applyTwoFilters`
}

// contractPassParamDirectly passes an ordinary parameter, which is mutable under
// Phase 1b (the caller cannot know its clone state).
func contractPassParamDirectly(db *gorm.DB) {
	applyTwoFilters(db) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of applyTwoFilters`
}

// ===== SHOULD NOT REPORT =====

// contractPassIsolated passes a Session-isolated (immutable) value.
func contractPassIsolated(root *gorm.DB) {
	s := root.Session(&gorm.Session{})
	applyTwoFilters(s) // OK: s is immutable
}

// contractForwardParam is itself immutable-param, so its parameter is immutable
// inside it and may be forwarded safely. It also branches the parameter, so its
// own directive is non-redundant.
//
//gormreuse:immutable-param
func contractForwardParam(db *gorm.DB) {
	db.Where("local").Find(nil)
	applyTwoFilters(db) // OK: db is immutable inside this function
}

// contractRedundantCalleeNoContract calls a redundant immutable-param helper
// (helper never branches its param, so it imposes no caller-side obligation).
func contractRedundantCalleeNoContract(root *gorm.DB) {
	applyOneFilter(root.Session(&gorm.Session{}).Where("x")) // OK: helper does not rely on immutability
}

// applyOneFilter is annotated immutable-param but only uses its parameter once,
// so the directive is redundant and imposes no caller-side contract.
//
//gormreuse:immutable-param
func applyOneFilter(db *gorm.DB) { // want `redundant gormreuse:immutable-param directive: no \*gorm\.DB parameter is reused`
	db.Where("a").Find(nil)
}
