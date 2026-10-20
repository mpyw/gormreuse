package internal

import "gorm.io/gorm"

// Fixtures for issue #346: under //gormreuse:immutable-param a parameter is
// isolated, so a discarded chain on it changes a copy and has no effect.
// Such a statement is reported. Without the directive it would change the
// caller's value, so no fix isolates an argument to such a function: the
// function's own queries would lose the condition.

// ===== NO FIX =====

//gormreuse:immutable-param
func discardApplyTenant(tx *gorm.DB) {
	tx.Where("t = 1") // want `immutable-param declared but the result of tx\.Where is discarded`
	tx.Find(nil)
	tx.Count(nil)
}

//gormreuse:immutable-param
func discardApplyChain(tx *gorm.DB) {
	tx.Where("t = 1").Limit(1) // want `immutable-param declared but the result of tx\.Where is discarded`
	tx.Find(nil)
	tx.Count(nil)
}

func discardCaller(db, other *gorm.DB) {
	q := db.Where("a")
	discardApplyTenant(q) // want `mutable \*gorm\.DB passed to discardApplyTenant, which has a gormreuse:immutable-param directive`
	r := other.Where("b")
	discardApplyChain(r) // want `mutable \*gorm\.DB passed to discardApplyChain, which has a gormreuse:immutable-param directive`
}

// ===== NOT REPORTED =====

// discardNone reads every chain it builds: a finisher ends it, or the
// result is assigned. A discarded Session changes nothing.
//
//gormreuse:immutable-param
func discardNone(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	t := tx.Where("b")
	t.Find(nil)
	tx.Session(&gorm.Session{})
	tx.Count(nil)
}

// discardNoDirective has no directive, so its discarded Where changes its
// caller's value, as it means to.
func discardNoDirective(tx *gorm.DB) {
	tx.Where("t = 1")
}

// ===== FIXED =====

func discardNoneCaller(db *gorm.DB) {
	q := db.Where("a")
	discardNone(q) // want `mutable \*gorm\.DB passed to discardNone, which has a gormreuse:immutable-param directive`
}
