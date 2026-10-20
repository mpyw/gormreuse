package internal

import (
	"gorm.io/gorm"
)

// Fixtures for issue #313: a reused *gorm.DB parameter of a closure gets the
// body Session, `tx = tx.Session(&gorm.Session{})`, as a parameter of a
// function declaration does (#201). A closure is never declared
// immutable-param. TestFixesConverge re-lints the golden. The closures that
// get no fix are in fix_param_root.go.

// fixClosureWithTx is a Transaction wrapper, the most common shape in gorm's
// own tests. Its callback's parameter is a mutable root.
func fixClosureWithTx(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	return db.Transaction(fn)
}

func fixClosureTxWrapper(db *gorm.DB) error {
	return fixClosureWithTx(db, func(tx *gorm.DB) error {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		return nil
	})
}

// fixClosureIIFE is called where it is written.
func fixClosureIIFE(db *gorm.DB) {
	func(tx *gorm.DB) {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}(db)
}

// fixClosureGo runs in a goroutine.
func fixClosureGo(db *gorm.DB) {
	go func(tx *gorm.DB) {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}(db.Where("x"))
}

// fixClosureDefer runs when the function returns.
func fixClosureDefer(db *gorm.DB) {
	defer func(tx *gorm.DB) {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}(db.Where("x"))
}

// fixClosureLocal is stored in a local and called twice.
func fixClosureLocal(a, b *gorm.DB) {
	f := func(tx *gorm.DB) {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	f(a)
	f(b.Where("x"))
}

// fixClosureOneLine: the Session goes on the same line as the body.
func fixClosureOneLine(db *gorm.DB) {
	f := func(tx *gorm.DB) { tx.Find(nil); tx.Count(nil) } // want `\*gorm\.DB reused: second branch from mutable root`
	f(db)
}

// fixClosureAlias reuses the parameter through an alias. The Session at the
// top of the body covers the alias too.
func fixClosureAlias(db *gorm.DB) {
	f := func(tx *gorm.DB) {
		r := tx
		r.Where("a").Find(nil)
		tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	f(db)
}

// fixClosureMerged is the pagination shape: the Where that the parameter
// merges with gets its own Session (#311).
func fixClosureMerged(db *gorm.DB, name string) {
	f := func(tx *gorm.DB) {
		if name != "" {
			tx = tx.Where("name = ?", name)
		}
		tx.Count(nil)
		tx.Offset(10).Limit(10).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	f(db)
}

// fixClosureNested is a closure in a closure.
func fixClosureNested(db *gorm.DB) error {
	return fixClosureWithTx(db, func(outer *gorm.DB) error {
		return fixClosureWithTx(outer.Session(&gorm.Session{}), func(tx *gorm.DB) error {
			tx.Where("a").Find(nil)
			tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			return nil
		})
	})
}

// FixClosurePackageLevel is a package-level closure, which another package
// may call.
var FixClosurePackageLevel = func(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
