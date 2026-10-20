package converge

import "gorm.io/gorm"

// Issue #309: a root used only in go and defer statements is fixed in one
// pass with a Session at the root.

// gdoGoOnly: two goroutines branch the root.
func gdoGoOnly(db *gorm.DB) {
	q := db.Where("x")
	go q.Count(nil)
	go q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// gdoDeferOnly: two deferred calls branch the root.
func gdoDeferOnly(db *gorm.DB) {
	q := db.Where("x")
	defer q.Count(nil)
	defer q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// gdoGoInLoop: one goroutine per iteration branches the root.
func gdoGoInLoop(db *gorm.DB, ids []int) {
	q := db.Where("x")
	for range ids {
		go q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}
