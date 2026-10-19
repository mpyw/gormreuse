package converge

import "gorm.io/gorm"

// Issues #249, #252 and #247: a use that runs more than once is fixed in one
// pass. The fix asks the checker's cfg.Repeats which uses run again.

// rpForPost: the post statement runs after the body, in the same block.
func rpForPost(db *gorm.DB) {
	q := db.Where("x")
	for i := 0; i < 2; q.Where("a") { // want `\*gorm\.DB reused: second branch from mutable root`
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		i++
	}
}

// rpClosureCalledTwice: the closure's one use runs twice.
func rpClosureCalledTwice(db *gorm.DB) {
	q := db.Where("x")
	f := func() {
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	f()
	f()
}

// rpIIFEInLoop: the IIFE runs on each iteration.
func rpIIFEInLoop(db *gorm.DB, ids []int) {
	q := db.Where("x")
	for range ids {
		func() {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		}()
	}
}

// rpNestedLoop: the use before the inner loop is in the outer loop (#252).
func rpNestedLoop(db *gorm.DB, n int) {
	q := db.Where("x")
	for i := 0; i < n; i++ {
		if i > 2 {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		}
		for j := 0; j < n; j++ {
		}
	}
}

// rpOuterLoopRoot: a root of the outer loop is used on each inner iteration.
func rpOuterLoopRoot(db *gorm.DB, n int) {
	db = db.Session(&gorm.Session{})
	for i := 0; i < n; i++ {
		q := db.Where("x")
		for j := 0; j < n; j++ {
			q.Where("a").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		}
	}
}

// rpReassignInLoopRoot: the reassignment of a root that the loop defines
// again needs no Session: the next iteration does not read it.
func rpReassignInLoopRoot(db *gorm.DB, n int) {
	db = db.Session(&gorm.Session{})
	for i := 0; i < n; i++ {
		q := db.Where("x")
		q.Where("a")
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}
