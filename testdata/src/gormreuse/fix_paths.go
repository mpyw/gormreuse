package internal

import "gorm.io/gorm"

// Fixes that follow control flow and add no redundant Session (#202).

// ===== SHOULD REPORT =====

// fixPathsReassign is the README's reassignment example. The reassigned value
// is used once, so it gets no Session.
func fixPathsReassign(db *gorm.DB) {
	q := db.Where("base")
	q.Where("a")
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixPathsCombined is the README's combined example. Only the value made by
// the first reassignment is used twice.
func fixPathsCombined(db *gorm.DB) {
	q := db.Where("base")
	q.Where("a")
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Where("c")           // want `\*gorm\.DB reused: second branch from mutable root`
	q.Where("d").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixPathsIfWithoutElse discards a statement in an if without an else. The
// path that skips it uses the original root twice, so the root gets a Session
// as well as the reassignment.
func fixPathsIfWithoutElse(db *gorm.DB, c bool) {
	q := db.Where("base")
	if c {
		q.Where("a")
	}
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Where("c").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixPathsReassignInLoop reassigns in a loop and uses the result after it. On
// a path through the loop the use sees the new value, so the original root
// needs no Session.
func fixPathsReassignInLoop(db *gorm.DB, xs []int) {
	q := db.Where("base")
	for _, x := range xs {
		q.Where("x = ?", x) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixPathsPhiAfterUses uses the root twice and then merges it with a value
// that is used once. The root gets a Session; the merge needs none.
func fixPathsPhiAfterUses(db *gorm.DB, c bool) {
	base := db.Session(&gorm.Session{})
	q := base.Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	if c {
		q = base.Where("z")
	}
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixPathsPhiCaptured is fixPathsPhiAfterUses with the last use in a closure,
// so q lives in memory and the merge is a pair of stores.
func fixPathsPhiCaptured(db *gorm.DB, c bool) {
	base := db.Session(&gorm.Session{})
	q := base.Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	if c {
		q = base.Where("z")
	}
	fn := func() { q.Find(nil) }
	fn() // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixPathsParenStatement discards a chain written in parentheses. It is
// reassigned like any other.
func fixPathsParenStatement(db *gorm.DB) {
	q := db.Where("base")
	(q.Where("a"))
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Where("c").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixPathsUsedInLoop uses a root defined before a loop once in the loop body.
// The next iteration uses it again, so it gets a Session.
func fixPathsUsedInLoop(db *gorm.DB, xs []int) {
	q := db.Where("base")
	for range xs {
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}
