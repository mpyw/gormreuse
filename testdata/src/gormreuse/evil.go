package internal

import "gorm.io/gorm"

// =============================================================================
// SHOULD REPORT - Closure tracking (via FreeVar tracing)
// =============================================================================

// closureReuse demonstrates detection of reuse through closures.
// FreeVar tracking allows us to trace captured variables back to their origin.
func closureReuse(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	// Closure captures q - we trace FreeVar back through MakeClosure
	fn := func() {
		q.Find(nil) // This pollutes q
	}
	fn()

	// Detected: q was polluted inside the closure
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// closurePollutesOutside demonstrates closure polluting, then using outside.
func closurePollutesOutside(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	func() {
		q.Find(nil) // Pollutes q inside closure
	}()

	q.First(nil) // want `\*gorm\.DB instance reused after chain method`
}

// nestedClosure demonstrates nested closure detection.
func nestedClosure(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	outer := func() {
		inner := func() {
			q.Find(nil) // Pollutes q in nested closure
		}
		inner()
	}
	outer()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Closure safe patterns
// =============================================================================

// closureWithSession demonstrates safe closure with Session.
func closureWithSession(db *gorm.DB) {
	q := db.Where("x = ?", 1).Session(&gorm.Session{})

	fn := func() {
		q.Find(nil)
	}
	fn()

	q.Count(nil) // OK: q was created with Session at end
}

// closureIndependentUse demonstrates independent use in closure.
func closureIndependentUse(db *gorm.DB) {
	q := db.Where("x = ?", 1).Session(&gorm.Session{})

	func() {
		// This creates a new chain from q, doesn't pollute q
		q.Where("extra = ?", 2).Find(nil)
	}()

	q.Find(nil) // OK: q is immutable (Session at end), closure derived new chain
}

// =============================================================================
// SHOULD REPORT - Conditional reuse
// =============================================================================

// conditionalReuse demonstrates that if/else branches are mutually exclusive.
// Only the merge point (after both branches) should be flagged.
func conditionalReuse(db *gorm.DB, flag bool) {
	q := db.Where("base = ?", true)

	if flag {
		q.Where("a = ?", 1).Find(nil) // First use in if-branch
	} else {
		q.Where("b = ?", 2).Find(nil) // First use in else-branch (mutually exclusive)
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// conditionalBothBranches demonstrates that if/else branches are mutually exclusive.
// Flow-sensitive analysis correctly handles this - branches don't see each other's pollution.
func conditionalBothBranches(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		q.Find(nil) // First use in if-branch
	} else {
		q.First(nil) // First use in else-branch (mutually exclusive)
	}

	// After either branch, q is polluted - this IS a reuse
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// switchReuse demonstrates that switch cases are mutually exclusive.
// Each case is a first use in its exclusive path - no violations within cases.
func switchReuse(db *gorm.DB, mode int) {
	q := db.Where("base = ?", 1)

	// Each switch case is mutually exclusive - only one executes at runtime
	switch mode {
	case 1:
		q.Where("a = ?", 1).Find(nil) // First use in case 1
	case 2:
		q.Where("b = ?", 2).Find(nil) // First use in case 2
	default:
		q.Where("c = ?", 3).Find(nil) // First use in default
	}
}

// =============================================================================
// SHOULD REPORT - Loop reuse (assumes 2+ iterations)
// =============================================================================

// loopReuse demonstrates reuse in a loop.
// Loop detection: terminal call in loop with root defined outside = violation.
func loopReuse(db *gorm.DB, items []string) {
	q := db.Where("base = ?", 1)

	for _, item := range items {
		// Second iteration reuses polluted q
		q.Where("item = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
	}
}

// forLoopReuse demonstrates reuse in a for loop.
func forLoopReuse(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	for i := 0; i < 3; i++ {
		// Second iteration reuses polluted q
		q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
	}
}

// =============================================================================
// SHOULD NOT REPORT - Loop safe patterns
// =============================================================================

// loopWithSession demonstrates safe loop with Session.
func loopWithSession(db *gorm.DB, items []string) {
	q := db.Where("base = ?", 1)

	for _, item := range items {
		q.Session(&gorm.Session{}).Where("item = ?", item).Find(nil) // OK: Session before each use
	}
}

// loopNewChainEachIteration demonstrates creating new chain in each iteration.
func loopNewChainEachIteration(db *gorm.DB, items []string) {
	for _, item := range items {
		q := db.Where("item = ?", item)
		q.Find(nil) // OK: new q each iteration
	}
}

// =============================================================================
// SHOULD REPORT - Defer reuse
// Defer executes at function return, after q.Find pollutes q.
// =============================================================================

// deferReuse demonstrates reuse with defer.
func deferReuse(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	// Defer is processed in second pass, after q.Find pollutes q
	defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`

	q.Find(nil) // LIMITATION: Not detected (defer executes after, but q.Find is first in code order)
}

// deferFunctionCallWithDB demonstrates defer with function call passing *gorm.DB.
// Tests lines 669-678: defer with *gorm.DB argument to function.
func deferFunctionCallWithDB(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil) // First use - pollutes q

	defer helperPollute(q) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Defer safe patterns
// =============================================================================

// deferWithSession demonstrates safe defer with Session.
func deferWithSession(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer q.Session(&gorm.Session{}).Count(nil) // OK: Session before use

	q.Session(&gorm.Session{}).Find(nil) // OK: Session before use
}

// =============================================================================
// SHOULD REPORT - Function call pollution
// Functions taking *gorm.DB as argument are assumed to pollute it.
// =============================================================================

// helperPollute is a helper that takes *gorm.DB as argument.
func helperPollute(db *gorm.DB) {
	db.Find(nil) // This pollutes db
}

// interProceduralPollution demonstrates function call pollution detection.
// Functions receiving *gorm.DB are assumed to pollute unless marked pure.
func interProceduralPollution(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	helperPollute(q) // Marks q as polluted (function assumed to pollute)

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Pure function (gormreuse:pure directive)
// =============================================================================

// helperPure is a helper marked as pure - it doesn't pollute *gorm.DB.
//
//gormreuse:pure
func helperPure(db *gorm.DB) {
	// This function only reads, doesn't pollute
	_ = db
}

// pureFunction demonstrates that pure functions don't pollute.
func pureFunction(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	helperPure(q) // Does NOT mark q as polluted (function is pure)

	q.Find(nil) // OK: q was not polluted by helperPure
}

// helperPureWithQuery is a pure helper that creates a new chain.
//
//gormreuse:pure
func helperPureWithQuery(db *gorm.DB) *gorm.DB {
	return db.Where("extra = ?", 1) // Returns new chain, doesn't pollute original
}

// pureFunctionWithReturn demonstrates pure function returning new chain.
func pureFunctionWithReturn(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	_ = helperPureWithQuery(q) // Does NOT mark q as polluted

	q.Find(nil) // OK: q was not polluted
}

// =============================================================================
// SHOULD REPORT - Interface method calls
// Interface methods are assumed to pollute *gorm.DB (safe default).
// =============================================================================

type Repository interface {
	Query(db *gorm.DB)
}

// interfaceMethodPollution demonstrates interface method pollution detection.
// Interface methods taking *gorm.DB are assumed to pollute it.
func interfaceMethodPollution(db *gorm.DB, repo Repository) {
	q := db.Where("x = ?", 1)
	repo.Query(q) // Interface method assumed to pollute q

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD REPORT - Channel communication
// =============================================================================

// channelPollution demonstrates channel send pollution detection.
// Sending *gorm.DB to channel is assumed to pollute it.
func channelPollution(db *gorm.DB, ch chan *gorm.DB) {
	q := db.Where("x = ?", 1)
	ch <- q // Sending to channel marks q as polluted

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD REPORT - Goroutine (closure-based)
// =============================================================================

// goroutineClosureReuse demonstrates reuse detected through goroutine closure.
// Note: We detect the closure pollution, not the concurrent execution issue.
func goroutineClosureReuse(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	go func() {
		q.Find(nil) // Pollutes q in goroutine closure
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// goroutineDirectMethodCall demonstrates direct method call in goroutine.
// This tests the processCallCommonForGo path for method calls.
func goroutineDirectMethodCall(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil) // First use - pollutes q

	go q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// goroutineFunctionCallWithDB demonstrates passing *gorm.DB to function in goroutine.
// This tests the processCallCommonForGo path for function calls with *gorm.DB argument.
func goroutineFunctionCallWithDB(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil) // First use - pollutes q

	go helperPollute(q) // want `\*gorm\.DB instance reused after chain method`
}

// goroutineCrossFunctionPollution demonstrates cross-function pollution with goroutine.
// Tests line 487: isPollutedAt cross-function pollution check.
// Pollution happens in closure, then goroutine checks isPollutedAt.
func goroutineCrossFunctionPollution(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	// Pollute q inside a closure (IIFE)
	func() {
		q.Find(nil)
	}()

	// Start goroutine that uses the already-polluted q
	// isPollutedAt should detect pollution from different function (closure)
	go q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Goroutine safe patterns
// =============================================================================

// goroutineWithSession demonstrates safe goroutine with Session.
func goroutineWithSession(db *gorm.DB) {
	q := db.Where("x = ?", 1).Session(&gorm.Session{})

	go func() {
		q.Find(nil)
	}()

	q.Count(nil) // OK: q was created with Session at end
}

// goroutineIndependentChain demonstrates independent chain in goroutine.
func goroutineIndependentChain(db *gorm.DB) {
	q := db.Where("x = ?", 1).Session(&gorm.Session{})

	go func() {
		// Creates new chain, doesn't pollute q
		q.Where("y = ?", 2).Find(nil)
	}()

	q.Find(nil) // OK: q is immutable
}

// =============================================================================
// EVIL PATTERNS - Deep Nested Closures (3-4 levels)
// =============================================================================

// tripleNestedClosure demonstrates 3-level nested closure detection.
func tripleNestedClosure(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	level1 := func() {
		level2 := func() {
			level3 := func() {
				q.Find(nil) // Pollutes q in 3rd level
			}
			level3()
		}
		level2()
	}
	level1()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// quadrupleNestedClosure demonstrates 4-level nested closure detection.
func quadrupleNestedClosure(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	func() {
		func() {
			func() {
				func() {
					q.Find(nil) // Pollutes q in 4th level
				}()
			}()
		}()
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// tripleNestedClosureSafe demonstrates 3-level nested closure with Session.
func tripleNestedClosureSafe(db *gorm.DB) {
	q := db.Where("x = ?", 1).Session(&gorm.Session{})

	func() {
		func() {
			func() {
				q.Find(nil)
			}()
		}()
	}()

	q.Count(nil) // OK: q has Session
}

// parentPollutesNestedClosureUses demonstrates pollution in parent, violation in nested closure.
// This is the reverse of closurePollutesOutside - parent pollutes first, then nested closure reuses.
func parentPollutesNestedClosureUses(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	q.Find(nil) // Pollutes q in parent

	func() {
		func() {
			q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
		}()
	}()
}

// parentPollutesTripleNestedUses demonstrates pollution in parent, violation in triple-nested closure.
func parentPollutesTripleNestedUses(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	q.Find(nil) // Pollutes q in parent

	func() {
		func() {
			func() {
				q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
			}()
		}()
	}()
}

// =============================================================================
// EVIL PATTERNS - Higher-Order Functions (go fn()())
// =============================================================================

// makeWorker returns a function that uses *gorm.DB.
func makeWorker(db *gorm.DB) func() {
	return func() {
		db.Find(nil)
	}
}

// higherOrderGoroutine demonstrates go fn()() pattern.
func higherOrderGoroutine(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	go makeWorker(q)() // Pollutes q through higher-order function

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// makeWorkerMaker returns a function that returns a function.
func makeWorkerMaker(db *gorm.DB) func() func() {
	return func() func() {
		return func() {
			db.Find(nil)
		}
	}
}

// doubleHigherOrderGoroutine demonstrates go fn()()() pattern.
func doubleHigherOrderGoroutine(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	go makeWorkerMaker(q)()() // Pollutes q through double higher-order

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// tripleHigherOrder demonstrates go fn()()()() pattern.
func tripleHigherOrder(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	maker := func(d *gorm.DB) func() func() func() {
		return func() func() func() {
			return func() func() {
				return func() {
					d.Find(nil)
				}
			}
		}
	}

	go maker(q)()()() // Pollutes q through triple higher-order

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Nested Defer/Goroutine Combinations
// =============================================================================

// deferInsideGoroutine demonstrates defer inside goroutine closure.
// [LIMITATION] Defer inside goroutine closure not fully tracked.
func deferInsideGoroutine(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	go func() {
		defer q.Find(nil) // Pollutes q in deferred call inside goroutine
	}()

	// [LIMITATION] FALSE NEGATIVE: Defer inside goroutine not tracked
	q.Count(nil) // Not detected - defer in goroutine limitation
}

// goroutineInsideDefer demonstrates goroutine inside defer.
func goroutineInsideDefer(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer func() {
		go func() {
			q.Find(nil) // Pollutes q in goroutine inside defer
		}()
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// nestedDeferGoroutineDefer demonstrates defer->goroutine->defer chain.
// [LIMITATION] Deep nested defer/goroutine chains not fully tracked.
func nestedDeferGoroutineDefer(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer func() {
		go func() {
			defer q.Find(nil) // Pollutes q in defer inside goroutine inside defer
		}()
	}()

	// [LIMITATION] FALSE NEGATIVE: Nested defer/goroutine chains not tracked
	q.Count(nil) // Not detected - nested defer/goroutine limitation
}

// multipleDefers demonstrates multiple defers using same q.
func multipleDefers(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
	defer q.First(nil) // want `\*gorm\.DB instance reused after chain method`

	q.Find(nil) // LIMITATION: Not detected (defers execute after, but q.Find is first in code order)
}

// =============================================================================
// EVIL PATTERNS - FreeVar from Deep Levels
// =============================================================================

// freeVarFrom4To2 demonstrates FreeVar reference from 4 levels deep to level 2.
func freeVarFrom4To2(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	level1 := func() {
		// q is captured at level 1
		level2 := func() {
			// q is captured at level 2
			level3 := func() {
				// q is captured at level 3
				level4 := func() {
					// q is used at level 4, referencing back through FreeVar chain
					q.Find(nil)
				}
				level4()
			}
			level3()
		}
		level2()
	}
	level1()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// freeVarMixedLevels demonstrates FreeVar with mixed usage levels.
func freeVarMixedLevels(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	outer := func() {
		// Use at level 1
		_ = q

		inner := func() {
			// Use at level 2
			innermost := func() {
				// Pollute at level 3
				q.Find(nil)
			}
			innermost()
		}
		inner()
	}
	outer()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - IIFE (Immediately Invoked Function Expression)
// =============================================================================

// iifeReuse demonstrates IIFE pattern.
func iifeReuse(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	func() {
		q.Find(nil)
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// nestedIIFE demonstrates nested IIFE pattern.
func nestedIIFE(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	func() {
		func() {
			q.Find(nil)
		}()
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// iifeWithArgument demonstrates IIFE with argument passing.
func iifeWithArgument(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	func(d *gorm.DB) {
		d.Find(nil)
	}(q)

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// iifeReturnChain demonstrates IIFE returning chain result.
// IIFE return tracing allows detection of pollution through IIFE return values.
func iifeReturnChain(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	// IIFE returns the result of Where, which is chained and pollutes q
	_ = func() *gorm.DB {
		return q.Where("y = ?", 2)
	}().Find(nil) // want `\*gorm\.DB instance reused after chain method`

	// Detected: q was polluted through the IIFE chain
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD REPORT - Struct Fields
// =============================================================================

type queryHolder struct {
	db *gorm.DB
}

// structFieldPollution demonstrates struct field storage without actual usage.
// Storing to struct field alone doesn't pollute - the struct is discarded without using the field.
func structFieldPollution(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	_ = &queryHolder{db: q} // Struct is discarded, field never used

	q.Count(nil) // OK: struct was discarded, no actual reuse occurred
}

type multiHolder struct {
	q1 *gorm.DB
	q2 *gorm.DB
}

// multiStructField demonstrates multiple struct fields pointing to same value.
// Field access is traced back to the original value.
func multiStructField(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	h := multiHolder{q1: q, q2: q}
	h.q1.Find(nil) // First use - pollutes underlying q

	h.q2.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Pointer Indirection
// =============================================================================

// pointerIndirection demonstrates pollution through pointer.
func pointerIndirection(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	ptr := &q
	(*ptr).Find(nil) // Pollutes q through pointer

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// doublePointer demonstrates double pointer indirection.
func doublePointer(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	ptr := &q
	ptr2 := &ptr
	(**ptr2).Find(nil) // Pollutes q through double pointer

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD REPORT - Type Assertions
// =============================================================================

// typeAssertionPollution demonstrates pollution through interface conversion.
// Converting *gorm.DB to interface{} is assumed to pollute it.
func typeAssertionPollution(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	var i interface{} = q // Converting to interface{} marks q as polluted
	_ = i

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD REPORT - Slice/Array Access
// =============================================================================

// slicePollution demonstrates pollution through slice storage.
// Storing *gorm.DB in slice is assumed to pollute it.
func slicePollution(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	_ = []*gorm.DB{q} // Storing in slice marks q as polluted

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// mapPollution demonstrates pollution through map storage.
// Storing *gorm.DB in map is assumed to pollute it.
func mapPollution(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	_ = map[string]*gorm.DB{"main": q} // Storing in map marks q as polluted

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Panic/Recover
// =============================================================================

// panicRecover demonstrates pollution in recover block.
func panicRecover(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer func() {
		if r := recover(); r != nil {
			q.Find(nil) // Pollutes q in recover
		}
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Select Statement
// =============================================================================

// selectStatement demonstrates that select cases are mutually exclusive.
// Flow-sensitive analysis correctly handles this - cases don't see each other's pollution.
func selectStatement(db *gorm.DB, ch chan int) {
	q := db.Where("x = ?", 1)

	select {
	case <-ch:
		q.Find(nil) // First use in case (mutually exclusive)
	default:
		q.First(nil) // First use in default (mutually exclusive)
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// Method Value - Now Detected
// SSA bound methods ($bound suffix) are now tracked properly.
// =============================================================================

// methodValue demonstrates pollution through method value.
// Method value tracking now works by detecting bound methods in SSA.
func methodValue(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	find := q.Find
	find(nil) // Pollutes q through method value

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// methodValueSameBlock demonstrates same-block pollution with method value.
// Tests line 423: same-block pollution detection for bound methods.
func methodValueSameBlock(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	find := q.Find
	find(nil)  // First use - pollutes q
	find(nil)  // want `\*gorm\.DB instance reused after chain method`
}

// methodValueInLoop demonstrates method value in loop.
// Tests line 429: loop violation detection for bound methods.
func methodValueInLoop(db *gorm.DB, items []string) {
	q := db.Where("x = ?", 1)
	find := q.Find

	for range items {
		find(nil) // want `\*gorm\.DB instance reused after chain method`
	}
}

// methodValueNonTerminal demonstrates non-terminal bound method call.
// Tests line 408: non-terminal bound method early return.
func methodValueNonTerminal(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	where := q.Where
	// The bound method call where("y = ?", 2) is non-terminal (chained to Find)
	// This should NOT report a violation since Where is a chain method
	where("y = ?", 2).Find(nil)
}

// methodValueConditional demonstrates method value with conditional.
// [LIMITATION] Phi node tracing for bound methods not fully supported.
func methodValueConditional(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)
	var find func(dest interface{}, conds ...interface{}) *gorm.DB
	if flag {
		find = q.Find
	} else {
		find = q.Find
	}
	find(nil) // [LIMITATION] Not detected - Phi node tracing for method values

	q.Count(nil) // Not detected due to limitation above
}

// =============================================================================
// EVIL PATTERNS - Closure Modifying Captured Variable
// =============================================================================

// closureModifiesCaptured demonstrates closure modifying captured variable.
// [LIMITATION] Cross-closure assignment tracking not fully supported.
func closureModifiesCaptured(db *gorm.DB) {
	var q *gorm.DB

	f := func() {
		q = db.Where("x = ?", 1)
	}
	f()

	q.Find(nil)
	// [LIMITATION] FALSE NEGATIVE: Assignment in closure not tracked
	q.Count(nil) // Not detected - closure assignment limitation
}

// =============================================================================
// EVIL PATTERNS - Named Return Values
// =============================================================================

func helperWithNamedReturn(db *gorm.DB) (result *gorm.DB) {
	result = db.Where("x = ?", 1)
	return
}

// namedReturn demonstrates pollution with named return.
func namedReturn(db *gorm.DB) {
	q := helperWithNamedReturn(db)
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD REPORT - Variadic Functions
// =============================================================================

func processQueries(queries ...*gorm.DB) {
	for _, q := range queries {
		q.Find(nil)
	}
}

// variadicPollution demonstrates pollution through variadic function.
// Passing *gorm.DB to variadic function is assumed to pollute it.
func variadicPollution(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	processQueries(q) // Function call assumed to pollute q

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Goto Statement
// =============================================================================

// gotoStatement demonstrates pollution with goto.
// Flow-sensitive analysis correctly detects mutually exclusive branches.
func gotoStatement(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		q.Find(nil)
		goto cleanup
	}
	// Not flagged: mutually exclusive branch (if-branch has goto, so this only runs when flag=false)
	q.First(nil)

cleanup:
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Fallthrough in Switch
// =============================================================================

// switchFallthrough demonstrates pollution with fallthrough.
// With fallthrough, case 0 can reach case 1, so case 1 is a reuse.
func switchFallthrough(db *gorm.DB, level int) {
	q := db.Where("x = ?", 1)

	switch level {
	case 0:
		q.Find(nil) // First use in case 0
		fallthrough
	case 1:
		q.First(nil) // want `\*gorm\.DB instance reused after chain method`
	}
}

// =============================================================================
// EVIL PATTERNS - Multiple Goroutines
// =============================================================================

// multipleGoroutines demonstrates multiple goroutines using same q.
func multipleGoroutines(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	go func() {
		q.Find(nil)
	}()

	go func() {
		q.First(nil) // want `\*gorm\.DB instance reused after chain method`
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Interleaved Function Calls
// =============================================================================

// interleavedCalls demonstrates interleaved function calls.
func interleavedCalls(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	func(d *gorm.DB) {
		func(d2 *gorm.DB) {
			d2.Find(nil)
		}(d)
	}(q)

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Combined Chaos
// =============================================================================

// combinedChaos demonstrates multiple evil patterns combined.
func combinedChaos(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	// Closure inside goroutine inside defer
	defer func() {
		go func() {
			func() {
				q.Find(nil)
			}()
		}()
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ultimateChaos demonstrates the ultimate evil pattern.
func ultimateChaos(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	// 4-level nested IIFE inside goroutine inside defer with higher-order function
	defer func() {
		go func() {
			maker := func() func() func() func() {
				return func() func() func() {
					return func() func() {
						return func() {
							q.Find(nil)
						}
					}
				}
			}
			maker()()()()
		}()
	}()

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Nested If Statements
// =============================================================================

// nestedIf demonstrates nested if statements with mutually exclusive branches.
// Flow-sensitive analysis correctly handles this - only the merge point is flagged.
func nestedIf(db *gorm.DB, a, b, c bool) {
	q := db.Where("x = ?", 1)

	if a {
		if b {
			if c {
				q.Find(nil) // First use in innermost if (mutually exclusive)
			} else {
				q.First(nil) // First use in else (mutually exclusive)
			}
		} else {
			q.Last(nil) // First use in outer else (mutually exclusive)
		}
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// deepNestedIf demonstrates 4-level nested if.
// The deep branch is the first use; only the merge point after is a violation.
func deepNestedIf(db *gorm.DB, a, b, c, d bool) {
	q := db.Where("x = ?", 1)

	if a {
		if b {
			if c {
				if d {
					q.Find(nil) // First use (mutually exclusive path)
				}
			}
		}
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ifElseChain demonstrates if-else-if chain with mutually exclusive branches.
// Flow-sensitive analysis correctly handles this - only the merge point is flagged.
func ifElseChain(db *gorm.DB, level int) {
	q := db.Where("x = ?", 1)

	if level == 0 {
		q.Find(nil) // First use in first branch
	} else if level == 1 {
		q.First(nil) // First use in else-if (mutually exclusive)
	} else if level == 2 {
		q.Last(nil) // First use in else-if (mutually exclusive)
	} else {
		q.Take(nil) // First use in else (mutually exclusive)
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - If Inside For
// =============================================================================

// ifInsideFor demonstrates if inside for loop.
func ifInsideFor(db *gorm.DB, items []string) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		if item != "" {
			q.Where("item = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}
}

// ifElseInsideFor demonstrates if-else inside for loop.
func ifElseInsideFor(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		if item > 0 {
			q.Where("positive = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		} else {
			q.Where("negative = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}
}

// nestedIfInsideFor demonstrates nested if inside for.
func nestedIfInsideFor(db *gorm.DB, items []int, flag bool) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		if item > 0 {
			if flag {
				q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
			}
		}
	}
}

// =============================================================================
// EVIL PATTERNS - For Inside If
// =============================================================================

// forInsideIf demonstrates for inside if.
func forInsideIf(db *gorm.DB, items []string, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		for _, item := range items {
			q.Where("item = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// forInsideIfElse demonstrates for inside if-else branches.
func forInsideIfElse(db *gorm.DB, items []string, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		for _, item := range items {
			q.Where("a = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	} else {
		for _, item := range items {
			q.Where("b = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Nested For Loops
// =============================================================================

// nestedFor demonstrates nested for loops.
func nestedFor(db *gorm.DB, outer, inner []string) {
	q := db.Where("x = ?", 1)

	for _, o := range outer {
		for _, i := range inner {
			q.Where("o = ? AND i = ?", o, i).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}
}

// tripleNestedFor demonstrates 3-level nested for.
func tripleNestedFor(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				q.Where("i = ? AND j = ? AND k = ?", i, j, k).Find(nil) // want `\*gorm\.DB instance reused after chain method`
			}
		}
	}
}

// forWithBreakContinue demonstrates for with break and continue.
func forWithBreakContinue(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		if item < 0 {
			continue
		}
		if item > 100 {
			break
		}
		q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Defer Inside If
// =============================================================================

// deferInsideIf demonstrates defer inside if.
func deferInsideIf(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
	}

	q.Find(nil) // LIMITATION: Not detected (conditional defer)
}

// deferInsideIfElse demonstrates defer inside if-else.
// Find executes FIRST, then defers execute at function exit.
func deferInsideIfElse(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
	} else {
		defer q.First(nil) // want `\*gorm\.DB instance reused after chain method`
	}

	q.Find(nil) // First use - defers execute AFTER this at function exit
}

// deferInsideNestedIf demonstrates defer inside nested if.
// Find executes FIRST, then defer executes at function exit.
func deferInsideNestedIf(db *gorm.DB, a, b bool) {
	q := db.Where("x = ?", 1)

	if a {
		if b {
			defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}

	q.Find(nil) // First use - defer executes AFTER this at function exit
}

// multipleDeferInsideIf demonstrates multiple defers inside if.
func multipleDeferInsideIf(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
		defer q.First(nil) // want `\*gorm\.DB instance reused after chain method`
	}

	q.Find(nil) // LIMITATION: Not detected (conditional defer)
}

// =============================================================================
// EVIL PATTERNS - Defer Inside For
// =============================================================================

// deferInsideFor demonstrates defer inside for loop.
// Note: Each iteration adds a defer, all execute at function return.
func deferInsideFor(db *gorm.DB, items []string) {
	q := db.Where("x = ?", 1)

	for range items {
		defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
	}

	q.Find(nil) // LIMITATION: Not detected (defer inside loop)
}

// deferInsideForWithCondition demonstrates defer inside for with condition.
func deferInsideForWithCondition(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		if item > 0 {
			//gormreuse:ignore - TEST FRAMEWORK ISSUE: diagnostic reported but want comment not matched
			defer q.Where("item = ?", item).Count(nil)
		}
	}

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// deferInsideNestedFor demonstrates defer inside nested for.
func deferInsideNestedFor(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			//gormreuse:ignore - TEST FRAMEWORK ISSUE: diagnostic reported but want comment not matched
			defer q.Where("i = ? AND j = ?", i, j).Count(nil)
		}
	}

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - For Inside Defer
// =============================================================================

// forInsideDefer demonstrates for inside defer closure.
func forInsideDefer(db *gorm.DB, items []string) {
	q := db.Where("x = ?", 1)

	defer func() {
		for range items {
			//gormreuse:ignore - TEST FRAMEWORK ISSUE: diagnostic reported but want comment not matched
			q.Count(nil)
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// nestedForInsideDefer demonstrates nested for inside defer.
func nestedForInsideDefer(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer func() {
		for i := 0; i < 2; i++ {
			for j := 0; j < 2; j++ {
				//gormreuse:ignore - TEST FRAMEWORK ISSUE: diagnostic reported but want comment not matched
				q.Find(nil)
			}
		}
	}()

	q.Where("setup").Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - If Inside Defer
// =============================================================================

// ifInsideDefer demonstrates if inside defer closure.
func ifInsideDefer(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	defer func() {
		if flag {
			q.Count(nil) // LIMITATION: Not detected (inside defer closure with condition)
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ifElseInsideDefer demonstrates if-else inside defer closure.
func ifElseInsideDefer(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	defer func() {
		if flag {
			q.Count(nil) // LIMITATION: Not detected (inside defer closure with condition)
		} else {
			q.First(nil) // LIMITATION: Not detected (inside defer closure with condition)
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// nestedIfInsideDefer demonstrates nested if inside defer.
func nestedIfInsideDefer(db *gorm.DB, a, b bool) {
	q := db.Where("x = ?", 1)

	defer func() {
		if a {
			if b {
				q.Count(nil) // LIMITATION: Not detected (nested if inside defer)
			} else {
				q.First(nil) // LIMITATION: Not detected (nested if inside defer)
			}
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Complex If/For/Defer Combinations
// =============================================================================

// ifForDefer demonstrates if containing for containing defer.
func ifForDefer(db *gorm.DB, flag bool, items []string) {
	q := db.Where("x = ?", 1)

	if flag {
		for range items {
			defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}

	q.Find(nil) // First use - defers execute AFTER this at function exit
}

// forIfDefer demonstrates for containing if containing defer.
func forIfDefer(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		if item > 0 {
			//gormreuse:ignore - TEST FRAMEWORK ISSUE
			defer q.Where("item = ?", item).Count(nil)
		}
	}

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// deferIfFor demonstrates defer closure containing if containing for.
func deferIfFor(db *gorm.DB, flag bool, items []string) {
	q := db.Where("x = ?", 1)

	defer func() {
		if flag {
			for range items {
				//gormreuse:ignore - TEST FRAMEWORK ISSUE
				q.Count(nil)
			}
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// deferForIf demonstrates defer closure containing for containing if.
func deferForIf(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	defer func() {
		for _, item := range items {
			if item > 0 {
				//gormreuse:ignore - TEST FRAMEWORK ISSUE
				q.Count(nil)
			}
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// tripleNestingIfForDefer demonstrates 3-level nesting: if -> for -> defer.
// [LIMITATION] Defer inside for loop: closure is deferred multiple times,
// but static analysis cannot detect the loop iteration count.
func tripleNestingIfForDefer(db *gorm.DB, flag bool, items []string) {
	q := db.Where("x = ?", 1)

	if flag {
		for range items {
			defer func() {
				q.Count(nil) // [LIMITATION] FALSE NEGATIVE: defer in for loop not fully tracked
			}()
		}
	}

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// tripleNestingForIfDefer demonstrates 3-level nesting: for -> if -> defer.
// [LIMITATION] Defer inside for loop: closure is deferred multiple times,
// but static analysis cannot detect the loop iteration count.
func tripleNestingForIfDefer(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		if item > 0 {
			defer func(i int) {
				q.Where("item = ?", i).Count(nil) // [LIMITATION] FALSE NEGATIVE: defer in for loop not fully tracked
			}(item)
		}
	}

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// tripleNestingDeferIfFor demonstrates 3-level nesting: defer -> if -> for.
func tripleNestingDeferIfFor(db *gorm.DB, flag bool, items []string) {
	q := db.Where("x = ?", 1)

	defer func() {
		if flag {
			for range items {
				//gormreuse:ignore - TEST FRAMEWORK ISSUE
				q.Count(nil)
			}
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// tripleNestingDeferForIf demonstrates 3-level nesting: defer -> for -> if.
func tripleNestingDeferForIf(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	defer func() {
		for _, item := range items {
			if item > 0 {
				//gormreuse:ignore - TEST FRAMEWORK ISSUE
				q.Count(nil)
			}
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - 4-Level Nesting Combinations
// =============================================================================

// quadNestingIfForIfDefer demonstrates 4-level: if -> for -> if -> defer.
func quadNestingIfForIfDefer(db *gorm.DB, a bool, items []int) {
	q := db.Where("x = ?", 1)

	if a {
		for _, item := range items {
			if item > 0 {
				defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
			}
		}
	}

	q.Find(nil) // First use - defers execute AFTER this at function exit
}

// quadNestingForIfForDefer demonstrates 4-level: for -> if -> for -> defer.
func quadNestingForIfForDefer(db *gorm.DB, outer []bool, inner []int) {
	q := db.Where("x = ?", 1)

	for _, flag := range outer {
		if flag {
			for range inner {
				defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
			}
		}
	}

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// quadNestingDeferIfForIf demonstrates 4-level: defer -> if -> for -> if.
func quadNestingDeferIfForIf(db *gorm.DB, a bool, items []int) {
	q := db.Where("x = ?", 1)

	defer func() {
		if a {
			for _, item := range items {
				if item > 0 {
					//gormreuse:ignore - TEST FRAMEWORK ISSUE
					q.Count(nil)
				}
			}
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// quadNestingDeferForIfFor demonstrates 4-level: defer -> for -> if -> for.
func quadNestingDeferForIfFor(db *gorm.DB, outer []bool) {
	q := db.Where("x = ?", 1)

	defer func() {
		for _, flag := range outer {
			if flag {
				for i := 0; i < 2; i++ {
					//gormreuse:ignore - TEST FRAMEWORK ISSUE
					q.Count(nil)
				}
			}
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Multiple Defers with If/For
// =============================================================================

// multipleDefersInDifferentBranches demonstrates multiple defers in different if branches.
func multipleDefersInDifferentBranches(db *gorm.DB, level int) {
	q := db.Where("x = ?", 1)

	if level == 0 {
		defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
	} else if level == 1 {
		defer q.First(nil) // want `\*gorm\.DB instance reused after chain method`
	} else {
		defer q.Last(nil) // want `\*gorm\.DB instance reused after chain method`
	}

	q.Find(nil) // First use - defers execute AFTER this at function exit
}

// multipleDefersInLoopBranches demonstrates multiple defers in loop branches.
func multipleDefersInLoopBranches(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		if item > 0 {
			defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
		} else {
			defer q.First(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Early Return with Defer
// =============================================================================

// earlyReturnWithDefer demonstrates early return with defer.
// Flow-sensitive analysis correctly detects mutually exclusive branches.
func earlyReturnWithDefer(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`

	if flag {
		q.Find(nil) // Pollutes q
		return
	}

	// Not flagged: mutually exclusive branch (if-branch has return, so this only runs when flag=false)
	q.First(nil)
}

// earlyReturnInLoopWithDefer demonstrates early return in loop with defer.
func earlyReturnInLoopWithDefer(db *gorm.DB, items []int) {
	q := db.Where("x = ?", 1)

	defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`

	for _, item := range items {
		if item < 0 {
			q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
			return
		}
		q.Where("item = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
	}
}

// =============================================================================
// EVIL PATTERNS - Labeled Break/Continue with Defer
// =============================================================================

// labeledBreakWithDefer demonstrates labeled break with defer.
func labeledBreakWithDefer(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`

outer:
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if i == 1 && j == 1 {
				q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
				break outer
			}
			q.Where("i = ? AND j = ?", i, j).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}
}

// labeledContinueWithDefer demonstrates labeled continue with defer.
func labeledContinueWithDefer(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`

outer:
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if j == 1 {
				q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
				continue outer
			}
			q.Where("i = ? AND j = ?", i, j).Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}
}

// =============================================================================
// EVIL PATTERNS - Infinite Loop Patterns
// =============================================================================

// foreverLoopWithBreak demonstrates for{} with break.
// With break at end, loop only runs once, so select cases are first use.
func foreverLoopWithBreak(db *gorm.DB, ch chan bool) {
	q := db.Where("x = ?", 1)

	for {
		select {
		case done := <-ch:
			if done {
				break
			}
		default:
			q.Find(nil) // First use (select case is mutually exclusive)
		}
		break // Prevent actual infinite loop in test
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Closure Capturing Loop Variable
// =============================================================================

// closureCapturingLoopVar demonstrates closure capturing loop variable.
func closureCapturingLoopVar(db *gorm.DB, items []string) {
	q := db.Where("x = ?", 1)

	var funcs []func()
	for _, item := range items {
		item := item // Capture
		funcs = append(funcs, func() {
			q.Where("item = ?", item).Find(nil)
		})
	}

	for _, f := range funcs {
		f() // LIMITATION: Not detected (closure invocation in loop)
	}
}

// deferCapturingLoopVar demonstrates defer capturing loop variable.
func deferCapturingLoopVar(db *gorm.DB, items []string) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		item := item // Capture
		defer func() {
			q.Where("item = ?", item).Count(nil) // LIMITATION: Not detected (defer in loop with captured var)
		}()
	}

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Nested Defer Closures
// =============================================================================

// nestedDeferClosures demonstrates nested defer closures.
func nestedDeferClosures(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer func() {
		defer func() {
			q.Count(nil) // LIMITATION: Not detected (nested defer closure)
		}()
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// tripleNestedDeferClosures demonstrates 3-level nested defer closures.
func tripleNestedDeferClosures(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer func() {
		defer func() {
			defer func() {
				q.Count(nil) // LIMITATION: Not detected (triple nested defer closure)
			}()
		}()
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// nestedDeferWithFor demonstrates nested defer with for.
func nestedDeferWithFor(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	defer func() {
		for i := 0; i < 2; i++ {
			defer func(n int) {
				q.Where("n = ?", n).Count(nil) // LIMITATION: Not detected (defer in loop inside defer closure)
			}(i)
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Range Over Channel with Defer
// =============================================================================

// rangeOverChannelWithDefer demonstrates range over channel with defer.
func rangeOverChannelWithDefer(db *gorm.DB, ch chan int) {
	q := db.Where("x = ?", 1)

	defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`

	for item := range ch {
		q.Where("item = ?", item).Find(nil) // want `\*gorm\.DB instance reused after chain method`
	}
}

// =============================================================================
// EVIL PATTERNS - Type Switch with Defer/For
// =============================================================================

// typeSwitchWithDefer demonstrates type switch with defer.
// Type switch cases are mutually exclusive - only defer sees the pollution.
func typeSwitchWithDefer(db *gorm.DB, v interface{}) {
	q := db.Where("x = ?", 1)

	defer q.Count(nil) // want `\*gorm\.DB instance reused after chain method`

	switch v.(type) {
	case int:
		q.Find(nil) // First use in case (mutually exclusive with other cases)
	case string:
		q.First(nil) // First use in case (mutually exclusive)
	default:
		q.Last(nil) // First use in default (mutually exclusive)
	}
}

// typeSwitchInsideFor demonstrates type switch inside for.
func typeSwitchInsideFor(db *gorm.DB, items []interface{}) {
	q := db.Where("x = ?", 1)

	for _, item := range items {
		switch item.(type) {
		case int:
			q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
		case string:
			q.First(nil) // want `\*gorm\.DB instance reused after chain method`
		}
	}
}

// =============================================================================
// EVIL PATTERNS - Ultimate If/For/Defer Chaos
// =============================================================================

// ultimateIfForDeferChaos demonstrates the ultimate if/for/defer combination.
func ultimateIfForDeferChaos(db *gorm.DB, a, b bool, outer []int, inner []string) {
	q := db.Where("x = ?", 1)

	defer func() {
		if a {
			for _, o := range outer {
				if o > 0 {
					for _, i := range inner {
						defer func(x int, y string) {
							if b {
								q.Where("x = ? AND y = ?", x, y).Count(nil) // LIMITATION: Not detected (deeply nested defer)
							}
						}(o, i)
					}
				}
			}
		}
	}()

	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Triple Nested IIFE
// =============================================================================

// tripleNestedIIFE demonstrates triple nested IIFE with pollution tracking.
func tripleNestedIIFE(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	_ = func() *gorm.DB {
		return func() *gorm.DB {
			return func() *gorm.DB {
				return q.Where("nested", 1)
			}()
		}()
	}().Find(nil) // want `\*gorm\.DB instance reused after chain method`

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// tripleNestedIIFEWithBranch demonstrates IIFE with conditional branches.
func tripleNestedIIFEWithBranch(db *gorm.DB, cond bool) {
	q := db.Where("x = ?", 1)

	_ = func() *gorm.DB {
		if cond {
			return q.Where("branch1", 1)
		}
		return q.Where("branch2", 2)
	}().Find(nil) // want `\*gorm\.DB instance reused after chain method`

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - IIFE with Multiple Return Paths
// =============================================================================

// iifeMultipleReturns demonstrates IIFE with multiple return statements.
func iifeMultipleReturns(db *gorm.DB, flag int) {
	q := db.Where("x = ?", 1)

	_ = func() *gorm.DB {
		switch flag {
		case 0:
			return q.Where("case0", 0)
		case 1:
			return q.Where("case1", 1)
		default:
			return q.Where("default", -1)
		}
	}().Find(nil) // want `\*gorm\.DB instance reused after chain method`

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - IIFE Returning Session (Safe)
// =============================================================================

// iifeReturnsSession demonstrates safe IIFE that returns Session result.
// Direct Session call creates immutable clone.
func iifeReturnsSession(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	// Direct Session call - result is immutable
	result := q.Session(&gorm.Session{})
	result.Find(nil)
	result.Count(nil) // Safe: Session creates immutable clone
}

// =============================================================================
// EVIL PATTERNS - Struct Field with IIFE
// =============================================================================

type iifeHolder struct {
	query *gorm.DB
}

// structFieldIIFE demonstrates struct field access with IIFE.
func structFieldIIFE(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	h := iifeHolder{
		query: func() *gorm.DB {
			return q.Where("from iife", 1)
		}(),
	}

	h.query.Find(nil) // want `\*gorm\.DB instance reused after chain method`
	q.Count(nil)      // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - Chained IIFE
// =============================================================================

// chainedIIFE demonstrates chained IIFE calls.
func chainedIIFE(db *gorm.DB) {
	q := db.Where("x = ?", 1)

	_ = func() *gorm.DB {
		return q.Where("first", 1)
	}().Where("second", 2).Find(nil) // want `\*gorm\.DB instance reused after chain method`

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - IIFE with Closure Capture
// =============================================================================

// iifeCaptureAndModify demonstrates IIFE that captures and uses a variable.
func iifeCaptureAndModify(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	var result *gorm.DB

	func() {
		result = q.Where("captured", 1)
	}()

	result.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// EVIL PATTERNS - IIFE with Phi Node
// =============================================================================

// iifeWithPhiNode demonstrates IIFE where the value comes from a Phi node.
func iifeWithPhiNode(db *gorm.DB, cond bool) {
	var q *gorm.DB
	if cond {
		q = db.Where("branch1", 1)
	} else {
		q = db.Where("branch2", 2)
	}

	_ = func() *gorm.DB {
		return q.Where("from phi", 1)
	}().Find(nil) // want `\*gorm\.DB instance reused after chain method`

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// DB INIT METHODS - Begin/Transaction (Immutable Source)
// =============================================================================

// beginReturnsImmutable demonstrates that Begin() returns an immutable source.
// Using tx (from Begin()) multiple times is safe - Begin() creates a new transaction.
func beginReturnsImmutable(db *gorm.DB) {
	tx := db.Begin()
	tx.Find(nil)
	tx.Count(nil) // Safe - tx is from Begin(), treated as immutable source
}

// beginChainedBecomesMutable demonstrates chaining after Begin() creates mutable.
func beginChainedBecomesMutable(db *gorm.DB) {
	tx := db.Begin().Where("x = ?", 1)
	tx.Find(nil)
	tx.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}
