package internal

import (
	"iter"

	"gorm.io/gorm"
)

// =============================================================================
// RANGE-OVER-FUNC EXITS (#399)
//
// go/ssa compiles the end of a range-over-func body and a continue into
// `return true`, so the iterator calls the body again. A break, a return or a
// jump out of the loop is `return false`, and the iterator must stop. So a use
// runs again only when it can reach the end of the body or a continue. A use
// that only reaches a break or a return runs once, as in a plain loop.
// =============================================================================

// =============================================================================
// SHOULD REPORT
// =============================================================================

// rangeFuncExitContinue: a continue runs the body again.
func rangeFuncExitContinue(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		if v > 0 {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			continue
		}
	}
}

// rangeFuncExitBreakAfterCheck: the use runs before the check that breaks, so
// it also reaches the end of the body.
func rangeFuncExitBreakAfterCheck(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		if v > 0 {
			break
		}
	}
}

// rangeFuncExitBreakOnSomePaths: one path from the use breaks, the other goes
// on to the next iteration.
func rangeFuncExitBreakOnSomePaths(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		if v > 0 {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			if v > 5 {
				break
			}
		}
	}
}

// rangeFuncExitInnerPlainLoop: a use in a plain loop inside the body repeats,
// even when the body breaks after that loop.
func rangeFuncExitInnerPlainLoop(db *gorm.DB, seq iter.Seq[int], xs []int) {
	q := db.Where("x")
	for range seq {
		for range xs {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		}
		break
	}
}

// rangeFuncExitAfterInnerPlainLoop: a use after a plain loop inside the body
// reaches the end of the body.
func rangeFuncExitAfterInnerPlainLoop(db *gorm.DB, seq iter.Seq[int], xs []int) {
	q := db.Where("x")
	for range seq {
		for range xs {
		}
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// rangeFuncExitInnerBreakOnly: the break leaves only the inner loop. The outer
// body goes on to its next iteration.
func rangeFuncExitInnerBreakOnly(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
	for range outer {
		for range inner {
			break
		}
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// rangeFuncExitLabeledBreak: [LIMITATION] a break to the outer label leaves
// both loops, so the use runs once. The inner body is a closure made in the
// outer body, and the outer body may run again, so the inner body is taken to
// run again with it. Which exit the outer body takes after the inner loop
// depends on a state variable that go/ssa sets in the inner body.
func rangeFuncExitLabeledBreak(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
outerLoop:
	for range outer {
		for b := range inner {
			if b > 0 {
				q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
				break outerLoop
			}
		}
	}
}

// rangeFuncExitNestedReturn: [LIMITATION] a return in the inner body leaves
// both loops, as in rangeFuncExitLabeledBreak.
func rangeFuncExitNestedReturn(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
	for range outer {
		for b := range inner {
			if b > 0 {
				q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
				return
			}
		}
	}
}

// =============================================================================
// SHOULD NOT REPORT
// =============================================================================

// rangeFuncExitBreak: the use is followed by a break, so it runs once.
func rangeFuncExitBreak(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		if v > 0 {
			q.Find(nil)
			break
		}
	}
}

// rangeFuncExitBreakUnconditional: the body always breaks.
func rangeFuncExitBreakUnconditional(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for range seq {
		q.Find(nil)
		break
	}
}

// rangeFuncExitReturn: a return leaves the loop and the function.
func rangeFuncExitReturn(db *gorm.DB, seq iter.Seq[int]) error {
	q := db.Where("x")
	for v := range seq {
		if v > 0 {
			return q.Find(nil).Error
		}
	}
	return nil
}

// rangeFuncExitAfterInnerPlainLoopBreak: the use after a plain loop inside the
// body is followed by a break.
func rangeFuncExitAfterInnerPlainLoopBreak(db *gorm.DB, seq iter.Seq[int], xs []int) {
	q := db.Where("x")
	for range seq {
		for range xs {
		}
		q.Find(nil)
		break
	}
}

// rangeFuncExitClosureBeforeBreak: a closure called once before the break runs
// once.
func rangeFuncExitClosureBeforeBreak(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		if v > 0 {
			func() { q.Find(nil) }()
			break
		}
	}
}
