package converge

import "gorm.io/gorm"

// Issue #373: values merged through a Phi whose fixes needed a second -fix
// pass. One pass now resolves each.

// mopLoopAfterIf: the discarded Where("b") reads the Phi that the Where("a")
// flows into. The fix of the Where("a") reassigns it too, so it does not
// change a copy.
func mopLoopAfterIf(db *gorm.DB, c bool, items []int) {
	q := db.Model(&User{})
	for range items {
		if c {
			q = q.Where("a")
		} else {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		}
		q.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
	}
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// mopLaterLoopReadsMerge: the Phi of the first loop has one use, but the
// second loop runs it again, so the Where("b") edge needs a Session.
func mopLaterLoopReadsMerge(db *gorm.DB, c bool, n int) {
	q := db.Model(&User{})
	if c {
		q.Where("a")
	} else {
		q.Find(nil)
	}
	for range n {
		q = q.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
	}
	for i := range 2 {
		q.Where("i = ?", i).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// mopInnerLoopReassignment: the Where("x") of the next inner iteration reads
// the Where("r") of the iteration before, though the reassignment of
// q.Where("o") dominates it.
func mopInnerLoopReassignment(db *gorm.DB, c bool) {
	q := db.Model(&User{})
	for i := range 2 {
		q.Where("o = ?", i) // want `\*gorm\.DB reused: second branch from mutable root`
		for j := range 2 {
			q.Where("x").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			if c {
				q = q.Where("r = ?", j) // want `\*gorm\.DB reused: second branch from mutable root`
			}
		}
	}
}

// mopReassignmentIntoLoopMerge: q = q.Where("a"), written by the fix, takes
// the root's place on the edge of the loop's Phi, which the last loop reads
// on each iteration.
func mopReassignmentIntoLoopMerge(db *gorm.DB, c bool, items []int) {
	q := db.Model(&User{}).Where("x")
	if c {
		q.Where("a")
	} else {
		q.Find(nil)
	}
	for range items {
		q = q.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
	}
	for range items {
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// mopReassignmentIntoSwitchMerge: the reassignment of q.Where("b") takes the
// root's place on the Phi after the switch, so the Where("d") on its other
// edge needs a Session too.
func mopReassignmentIntoSwitchMerge(db *gorm.DB, c bool) {
	q := db.Model(&User{}).Where("x")
	q = q.Where("a")
	q.Where("b")
	switch {
	case c:
		q.Where("c").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	default:
		q = q.Where("d") // want `\*gorm\.DB reused: second branch from mutable root`
	}
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Find(nil)  // want `\*gorm\.DB reused: second branch from mutable root`
}

// mopDiscardedInLoopOnEdge: the discarded Where("d") reads the Where("c") that
// flows back into the loop's Phi.
func mopDiscardedInLoopOnEdge(db *gorm.DB, c bool, items []int) {
	q := db.Model(&User{}).Where("x")
	q.Where("a").Find(nil)
	q.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
	if c {
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		for range items {
			q = q.Where("c") // want `\*gorm\.DB reused: second branch from mutable root`
			q.Where("d")
		}
	}
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
