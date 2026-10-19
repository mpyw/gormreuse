package converge

import "gorm.io/gorm"

// Fixtures for issue #248: deleting an unused or redundant directive, or an
// unused ignore, resolves its diagnostic in one pass.

//gormreuse:immutable-return,immutable-param // the note stays
func convergeRedundantParam(db *gorm.DB) *gorm.DB { // want `redundant gormreuse:immutable-param directive: no \*gorm\.DB parameter is reused`
	return db.Session(&gorm.Session{})
}

//gormreuse:immutable-param
func convergeRedundantAlone(db *gorm.DB) { // want `redundant gormreuse:immutable-param directive: no \*gorm\.DB parameter is reused`
	db.Find(nil)
}

//gormreuse:immutable-input(cb),immutable-input(missing) // want `unused gormreuse:immutable-input directive: parameter "missing" not found`
func convergeUnusedInput(db *gorm.DB, cb func(*gorm.DB)) {
	cb(db.Session(&gorm.Session{}))
}

//gormreuse:ignore,pure // want `unused gormreuse:ignore directive`
func convergeUnusedIgnore(db *gorm.DB) {
	_ = db
}

//gormreuse:pure,immutable-return // want `unused gormreuse:pure directive` `unused gormreuse:immutable-return directive`
func convergeBothUnused(x int) int { return x }

func convergeTrailingIgnore() {
	_ = 1 //gormreuse:ignore // want `unused gormreuse:ignore directive`
}
