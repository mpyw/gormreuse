package converge

import (
	"context"

	"gorm.io/gorm"
)

// Parameter roots and Phi edges from issue #201. Each fix must leave nothing
// to report: a Session in the body where a caller may pass a mid-chain value,
// and a Session on a Phi edge whose last call is a chain method.

// ParamExported can be called from another package.
func ParamExported(tx *gorm.DB) { // want ParamExported:`gormreuse finisher-only\[0\]`
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

// paramIsolatedCaller has only callers that pass an isolated value, so it is
// inferred immutable-param, and the reuse is not reported (#349).
func paramIsolatedCaller(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil)
}

func paramCallsIsolated(db *gorm.DB) {
	paramIsolatedCaller(db.Where("scoped").Session(&gorm.Session{}))
}

// paramDiscardedResult changes its parameter in place, and returns a
// *gorm.DB that carries the change back to its caller (#243).
func paramDiscardedResult(tx *gorm.DB) *gorm.DB {
	tx.Where("a")
	tx.Find(nil)         // want `\*gorm\.DB reused: second branch from mutable root`
	return tx.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
}

func paramCallsDiscardedResult(db *gorm.DB) {
	paramDiscardedResult(db.Where("scoped")).Find(nil)
}

// paramTwoArgs is isolated through a, but its caller passes a mid-chain b.
// Declaring it immutable-param would report that argument, so a gets the
// body Session (#245).
func paramTwoArgs(a, b *gorm.DB) {
	a.Find(nil)
	a.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	b.Find(nil)
}

func paramCallsTwoArgs(db *gorm.DB) {
	paramTwoArgs(db.Session(&gorm.Session{}), db.Where("y"))
}

// paramClosureCaller is also called from a package-level closure, which
// passes a mid-chain value (#245).
func paramClosureCaller(q *gorm.DB) {
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

var paramRunClosure = func(db *gorm.DB) { paramClosureCaller(db.Where("x")) }

func paramCallsClosureCaller(db *gorm.DB) {
	paramClosureCaller(db.Session(&gorm.Session{}))
	paramRunClosure(db)
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
