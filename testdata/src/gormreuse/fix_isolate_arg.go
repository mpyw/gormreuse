package internal

import "gorm.io/gorm"

// Fixtures for issue #248: a mutable *gorm.DB passed to an immutable-param
// parameter gets .Session(&gorm.Session{}) after the argument.

//gormreuse:immutable-param
func isolateBranchBoth(a, b *gorm.DB) {
	a.Where("x").Find(nil)
	a.Where("y").Find(nil)
	b.Where("x").Find(nil)
	b.Where("y").Find(nil)
}

// isolateTwoArgs: one diagnostic stands for both arguments, so its fix
// isolates both.
func isolateTwoArgs(a, b *gorm.DB) {
	isolateBranchBoth(a.Where("a"), b.Where("b")) // want `mutable \*gorm\.DB passed to isolateBranchBoth, which has a gormreuse:immutable-param directive`
}

// isolateDeref: Session binds tighter than *, so the argument is wrapped.
func isolateDeref(db *gorm.DB) {
	p := &db
	isolateBranchBoth(*p, nil) // want `mutable \*gorm\.DB passed to isolateBranchBoth, which has a gormreuse:immutable-param directive`
}

// isolateShadowed: the local gorm shadows the import at the argument, so the
// fix imports gorm.io/gorm under another name (#237).
func isolateShadowed(db *gorm.DB) {
	gorm := db.Where("x")
	isolateBranchBoth(gorm, nil) // want `mutable \*gorm\.DB passed to isolateBranchBoth, which has a gormreuse:immutable-param directive`
}

type isolateRepo struct{}

//gormreuse:immutable-param
func (isolateRepo) apply(db *gorm.DB) {
	db.Where("x").Find(nil)
	db.Where("y").Find(nil)
}

// isolateMethod: the receiver is not an argument of the call expression.
func isolateMethod(db *gorm.DB) {
	isolateRepo{}.apply(db) // want `mutable \*gorm\.DB passed to apply, which has a gormreuse:immutable-param directive`
}
