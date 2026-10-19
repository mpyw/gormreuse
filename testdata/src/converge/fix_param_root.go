package converge

import (
	"context"

	"gorm.io/gorm"
)

// Parameter roots and Phi edges from issue #201. Each fix must leave nothing
// to report: a Session in the body where a caller may pass a mid-chain value,
// and a Session on a Phi edge whose last call is a chain method.

// ParamExported can be called from another package.
func ParamExported(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// paramMutableCaller has a caller that passes a mid-chain value.
func paramMutableCaller(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func paramCallsMutable(db *gorm.DB) {
	paramMutableCaller(db.Where("scoped"))
}

// paramIsolatedCaller has only callers that pass an isolated value.
func paramIsolatedCaller(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func paramCallsIsolated(db *gorm.DB) {
	paramIsolatedCaller(db.Where("scoped").Session(&gorm.Session{}))
}

// paramDiscardedResult changes its parameter in place, and returns a
// *gorm.DB that carries the change back to its caller (#243).
func paramDiscardedResult(tx *gorm.DB) *gorm.DB {
	tx.Where("a")
	tx.Find(nil)
	return tx.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
}

func paramCallsDiscardedResult(db *gorm.DB) {
	paramDiscardedResult(db.Where("scoped")).Find(nil)
}

// phiSessionMidChain is the issue's repro: Where after the Session makes the
// edge mutable.
//
//gormreuse:immutable-param
func phiSessionMidChain(db *gorm.DB, c bool) {
	q := db.Where("x")
	if c {
		q = db.Session(&gorm.Session{}).Where("y")
	}
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phiWithContextMidChain is the common WithContext form.
//
//gormreuse:immutable-param
func phiWithContextMidChain(ctx context.Context, db *gorm.DB, c bool) {
	q := db.Where("x")
	if c {
		q = db.WithContext(ctx).Where("y")
	}
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
