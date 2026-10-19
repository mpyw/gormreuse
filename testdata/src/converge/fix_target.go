package converge

import (
	"testing"

	"gorm.io/gorm"
)

// Issue #198: each fix must rewrite the variable that holds the reused root.
// Rewriting an enclosing receiver, an alias, or the receiver of a call that
// takes the root as an argument leaves the violation in place.

// fixTargetDB is a package-level handle that the fix must not overwrite.
var fixTargetDB *gorm.DB

// fixTargetPackageTransaction: the fix reassigns q, not fixTargetDB.
func fixTargetPackageTransaction() error {
	return fixTargetDB.Transaction(func(tx *gorm.DB) error {
		q := tx.Model(nil).Where("tenant = ?", 1)
		q.Where("active")
		q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		return nil
	})
}

// fixTargetTestingRun: the fix reassigns q, not t.
func fixTargetTestingRun(t *testing.T, db *gorm.DB) {
	t.Run("x", func(t *testing.T) {
		q := db.Where("x").Session(&gorm.Session{}).Where("y")
		q.Where("active")
		q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	})
}

// fixTargetAliasReceiver: the fix makes q's root immutable, not r = r.Where.
// A discarded r.Where("a") would get no fix at all (#199).
func fixTargetAliasReceiver(db *gorm.DB) {
	q := db.Where("x")
	r := q
	r.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixTargetArgument: the fix makes q's root immutable, not base = base.Or.
func fixTargetArgument(db, base *gorm.DB) {
	q := db.Where("x")
	base.Or(q)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixTargetGoroutine: the fix makes q's root immutable and writes nothing
// inside the goroutine. A discarded q.Where("a") there would get no fix at
// all (#199).
func fixTargetGoroutine(db *gorm.DB) {
	q := db.Where("x")
	go func() {
		q.Where("a").Find(nil)
		q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}()
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
