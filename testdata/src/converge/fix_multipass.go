package converge

import "gorm.io/gorm"

// Issue #247: fixes that needed a second -fix pass, or left a reuse that the
// re-lint no longer saw.

// mpCapturedPhi: q is captured, so SSA keeps it in memory with no Phi. The
// reads resolve to the first store, so only "a" got a Session, and the re-lint
// was clean while the "b" path still reused its root. Both stores get one.
func mpCapturedPhi(db *gorm.DB, c bool) {
	var q *gorm.DB
	if c {
		q = db.Where("a")
	} else {
		q = db.Where("b")
	}
	go func() {
		q.Find(nil)
		q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}()
}

// mpInlineClosure: the reassignment goes inside the closure, which detection
// does not follow, so the root gets its Session in the same pass.
func mpInlineClosure(db *gorm.DB) {
	q := db.Where("x")
	func() { q.Where("a") }()
	q.Find(nil)  // want `\*gorm\.DB reused: second branch from mutable root`
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// mpRangeFunc: the body of a range-over-func loop is a closure too.
func mpRangeFunc(db *gorm.DB, seq func(func(int) bool)) {
	q := db.Where("x")
	for range seq {
		q.Where("a") // want `\*gorm\.DB reused: second branch from mutable root`
	}
	q.Find(nil)  // want `\*gorm\.DB reused: second branch from mutable root`
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
