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
// that only reaches a break or a return runs once, as in a plain loop. In
// nested loops, the inner body stores which exit it took, and the code after
// the inner loop reads it, so a break out of both loops or a return also runs
// once.
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

// rangeFuncExitNestedInnerBreak: a break in the inner body leaves only the
// inner loop, and the outer body goes on to its next iteration.
func rangeFuncExitNestedInnerBreak(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
	for range outer {
		for range inner {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			break
		}
	}
}

// rangeFuncExitNestedContinueOuter: a continue to the outer label runs the
// outer body again.
func rangeFuncExitNestedContinueOuter(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
outerLoop:
	for range outer {
		for range inner {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			continue outerLoop
		}
	}
}

// rangeFuncExitBreakToMiddle: a break to the middle of three loops leaves the
// two inner ones, and the outermost body goes on.
func rangeFuncExitBreakToMiddle(db *gorm.DB, a, b, c iter.Seq[int]) {
	q := db.Where("x")
	for range a {
	middle:
		for range b {
			for range c {
				q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
				break middle
			}
		}
	}
}

// rangeFuncExitNestedOuterGoesOn: the outer body may go on after the inner
// loop, past a condition it cannot read.
func rangeFuncExitNestedOuterGoesOn(db *gorm.DB, outer, inner iter.Seq[int], done bool) {
	q := db.Where("x")
	for range outer {
		for range inner {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			break
		}
		if done {
			return
		}
	}
}

// rangeFuncExitInPlainLoop: the range-over-func loop runs on each iteration
// of a plain loop around it.
func rangeFuncExitInPlainLoop(db *gorm.DB, seq iter.Seq[int], n int) {
	q := db.Where("x")
	for i := 0; i < n; i++ {
		for range seq {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			break
		}
	}
}

// rangeFuncExitContinuePlainLoop: a continue to a plain loop around it runs
// the range-over-func loop again.
func rangeFuncExitContinuePlainLoop(db *gorm.DB, seq iter.Seq[int], n int) {
	q := db.Where("x")
plain:
	for i := 0; i < n; i++ {
		for range seq {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			continue plain
		}
	}
}

// rangeFuncExitInClosureCalledTwice: the closure that holds the loop runs
// twice.
func rangeFuncExitInClosureCalledTwice(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	f := func() {
		for range seq {
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			break
		}
	}
	f()
	f()
}

// rangeFuncExitSwitchBreak: a break in a switch leaves only the switch.
func rangeFuncExitSwitchBreak(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		switch v {
		case 1:
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			break
		}
	}
}

// rangeFuncExitSelectBreak: a break in a select leaves only the select.
func rangeFuncExitSelectBreak(db *gorm.DB, seq iter.Seq[int], ch chan int) {
	q := db.Where("x")
	for range seq {
		select {
		case <-ch:
			q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
			break
		}
	}
}

// rangeFuncExitStoreCapturedBreak: a closure stored into a captured variable
// before the break runs at each later call through the variable.
func rangeFuncExitStoreCapturedBreak(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	var f func()
	for range seq {
		f = func() { q.Find(nil) } // want `\*gorm\.DB reused: second branch from mutable root`
		break
	}
	f()
	f()
}

// rangeFuncExitStoreCapturedNested: the same from a nested loop that leaves
// both loops.
func rangeFuncExitStoreCapturedNested(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
	var f func()
	for range outer {
		for range inner {
			f = func() { q.Find(nil) } // want `\*gorm\.DB reused: second branch from mutable root`
			break
		}
		break
	}
	f()
	f()
}

// rangeFuncExitPassClosureBreak: a closure passed to a function before the
// break may run there any number of times.
func rangeFuncExitPassClosureBreak(db *gorm.DB, seq iter.Seq[int], run func(func())) {
	q := db.Where("x")
	for range seq {
		run(func() { q.Find(nil) }) // want `\*gorm\.DB reused: second branch from mutable root`
		break
	}
}

// rangeFuncExitSendClosureBreak: a closure sent on a channel before the
// break.
func rangeFuncExitSendClosureBreak(db *gorm.DB, seq iter.Seq[int], ch chan func()) {
	q := db.Where("x")
	for range seq {
		ch <- func() { q.Find(nil) } // want `\*gorm\.DB reused: second branch from mutable root`
		break
	}
}

// rangeFuncExitMapClosureReturn: a closure stored in a map before a return.
func rangeFuncExitMapClosureReturn(db *gorm.DB, seq iter.Seq[int], m map[int]func()) {
	q := db.Where("x")
	for range seq {
		m[0] = func() { q.Find(nil) } // want `\*gorm\.DB reused: second branch from mutable root`
		return
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

// rangeFuncExitLabeledBreak: a break to the outer label leaves both loops.
// The inner body stores the exit in a variable, and the outer body reads it
// and returns false too.
func rangeFuncExitLabeledBreak(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
outerLoop:
	for range outer {
		for b := range inner {
			if b > 0 {
				q.Find(nil)
				break outerLoop
			}
		}
	}
}

// rangeFuncExitNestedReturn: a return in the inner body leaves both loops.
func rangeFuncExitNestedReturn(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
	for range outer {
		for b := range inner {
			if b > 0 {
				q.Find(nil)
				return
			}
		}
	}
}

// rangeFuncExitTripleReturn: a return three loops deep leaves them all.
func rangeFuncExitTripleReturn(db *gorm.DB, a, b, c iter.Seq[int]) {
	q := db.Where("x")
	for range a {
		for range b {
			for range c {
				q.Find(nil)
				return
			}
		}
	}
}

// rangeFuncExitNestedThenBreak: the inner loop breaks, and the outer body
// then leaves on every path.
func rangeFuncExitNestedThenBreak(db *gorm.DB, outer, inner iter.Seq[int], done bool) {
	q := db.Where("x")
	for range outer {
		for range inner {
			q.Find(nil)
			break
		}
		if done {
			return
		}
		break
	}
}

// rangeFuncExitBreakPlainLoop: a break to a plain loop around it leaves that
// loop too.
func rangeFuncExitBreakPlainLoop(db *gorm.DB, seq iter.Seq[int], n int) {
	q := db.Where("x")
plain:
	for i := 0; i < n; i++ {
		for range seq {
			q.Find(nil)
			break plain
		}
	}
}

// rangeFuncExitRootInOuterBody: the root is defined in the outer body, so each
// run of it has its own root.
func rangeFuncExitRootInOuterBody(db *gorm.DB, outer, inner iter.Seq[int]) {
	for range outer {
		q := db.Session(&gorm.Session{}).Where("x")
		for range inner {
			q.Find(nil)
			break
		}
	}
}

// rangeFuncExitDeferBeforeBreak: a deferred call registered once before the
// break runs once.
func rangeFuncExitDeferBeforeBreak(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		if v > 0 {
			defer func() { q.Find(nil) }()
			break
		}
	}
}

// rangeFuncExitGoBeforeBreak: a goroutine spawned once before the break.
func rangeFuncExitGoBeforeBreak(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		if v > 0 {
			go q.Find(nil)
			break
		}
	}
}

// rangeFuncExitClosureVarBeforeBreak: a closure held in a variable and called
// once before the break.
func rangeFuncExitClosureVarBeforeBreak(db *gorm.DB, seq iter.Seq[int]) {
	q := db.Where("x")
	for v := range seq {
		if v > 0 {
			f := func() { q.Find(nil) }
			f()
			break
		}
	}
}

// rangeFuncExitBranchesThenLeaves: the use branches, and both ways leave the
// loop.
func rangeFuncExitBranchesThenLeaves(db *gorm.DB, outer, inner iter.Seq[int]) {
	q := db.Where("x")
	for range outer {
		for v := range inner {
			if v > 0 {
				q.Find(nil)
				if v > 5 {
					return
				}
				break
			}
		}
		return
	}
}
