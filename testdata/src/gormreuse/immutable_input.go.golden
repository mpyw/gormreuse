package internal

import "gorm.io/gorm"

// =============================================================================
// User-defined //gormreuse:immutable-input(name) Directive
// Issue #56 cases 2.1–2.4 + U1–U4.
// =============================================================================

// =============================================================================
// 2.1 — Builtin Transaction (already covered, kept for completeness)
// =============================================================================

// transactionCallbackUserDefined demonstrates that the existing builtin
// constraint still works after the user-defined wiring landed.
func transactionCallbackUserDefined(db *gorm.DB) {
	db = db.Session(&gorm.Session{}) // make parameter immutable
	_ = db.Transaction(func(tx *gorm.DB) error {
		tx.Find(nil)
		tx.Count(nil) // OK — tx is immutable input
		return nil
	})
}

// =============================================================================
// 2.2 — User-defined immutable-input on a free function
// =============================================================================

// withFreshDB declares that its callback receives an immutable *gorm.DB.
// The implementation respects this by handing fresh through Session().
//
//gormreuse:immutable-input(cb)
func withFreshDB(cb func(*gorm.DB) error, db *gorm.DB) error {
	fresh := db.Session(&gorm.Session{})
	return cb(fresh) // OK: passes immutable
}

// useWithFreshDBOK demonstrates that callbacks of immutable-input
// declarations may reuse their parameter.
func useWithFreshDBOK(db *gorm.DB) {
	db = db.Session(&gorm.Session{})
	_ = withFreshDB(func(q *gorm.DB) error {
		q.Find(nil)
		q.Count(nil) // OK — q is declared immutable
		return nil
	}, db)
}

// =============================================================================
// 2.3 — immutable-input declared but body passes a mutable value
// =============================================================================

// badImmutableInput violates its own immutable-input(cb) contract by
// handing the callback a mutable value derived from db.
//
//gormreuse:immutable-input(cb)
func badImmutableInput(cb func(*gorm.DB) error, db *gorm.DB) error {
	q := db.Where("x")                                                           // q is mutable
	return cb(q) // want `immutable-input\(cb\) declared but mutable \*gorm\.DB passed to callback`
}

// =============================================================================
// 2.4 — immutable-input declared and body passes an immutable value
// =============================================================================

// goodImmutableInput correctly hands an immutable value to its callback.
//
//gormreuse:immutable-input(cb)
func goodImmutableInput(cb func(*gorm.DB) error, db *gorm.DB) error {
	fresh := db.Session(&gorm.Session{})
	return cb(fresh)
}

// =============================================================================
// U1–U4 — Unused directive detection
// =============================================================================

// U1: parameter named `nonexistent` doesn't exist in the signature.
//
//gormreuse:immutable-input(nonexistent) // want `unused gormreuse:immutable-input directive: parameter "nonexistent" not found`
func noSuchParam(cb func(*gorm.DB)) {
	_ = cb
}

// U2: parameter `x` is not a function type.
//
//gormreuse:immutable-input(x) // want `unused gormreuse:immutable-input directive: parameter "x" is not a function type`
func notFuncType(x int) {
	_ = x
}

// U3: callback `cb` has no *gorm.DB parameter.
//
//gormreuse:immutable-input(cb) // want `unused gormreuse:immutable-input directive: callback "cb" has no \*gorm\.DB parameter`
func callbackNoGormDBParam(cb func(int) int) {
	_ = cb
}

// U4: valid declaration — no diagnostic.
//
//gormreuse:immutable-input(cb)
func validImmutableInput(cb func(*gorm.DB) error) error {
	return cb(nil)
}

// =============================================================================
// 2.5 — immutable-input on a METHOD (receiver-shifted parameter index)
// =============================================================================

type immutableInputRepo struct{ db *gorm.DB }

// withFreshMethod declares immutable-input(cb) on a method; cb is the first
// non-receiver parameter, so the index must be shifted past the receiver.
//
//gormreuse:immutable-input(cb)
func (r *immutableInputRepo) withFreshMethod(cb func(*gorm.DB) error) error {
	fresh := r.db.Session(&gorm.Session{})
	return cb(fresh) // OK: passes immutable
}

// badMethod violates its own method-level immutable-input(cb) contract.
//
//gormreuse:immutable-input(cb)
func (r *immutableInputRepo) badMethod(cb func(*gorm.DB) error, db *gorm.DB) error {
	q := db.Where("x")
	return cb(q) // want `immutable-input\(cb\) declared but mutable \*gorm\.DB passed to callback`
}

// useWithFreshMethod passes a closure to the method; its *gorm.DB parameter is
// treated as immutable input, so reuse inside it is OK (callback exemption with
// a receiver-shifted argument index).
func useWithFreshMethod(r *immutableInputRepo) {
	_ = r.withFreshMethod(func(q *gorm.DB) error {
		q.Find(nil)
		q.Count(nil) // OK — q is declared immutable input
		return nil
	})
}
