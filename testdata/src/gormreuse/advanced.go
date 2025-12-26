package internal

import (
	"gorm.io/gorm"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// SHOULD REPORT - Derived variables without Session
// =============================================================================

// derivedVariables demonstrates unsafe derived variables.
func derivedVariables(db *gorm.DB) {
	queryDB := db.Model(&User{}).Where("name = ?", "jinzhu").Session(&gorm.Session{})
	q := queryDB.Where("age > ?", 10) // Derived without Session - mutable
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB instance reused after chain method`
}

// storedChainResultReuse demonstrates reuse of stored chain result.
func storedChainResultReuse(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	q.Where("name = ?", "x").Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// storedChainResultMultipleDerivations demonstrates multiple derivations from same base.
func storedChainResultMultipleDerivations(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1)
	base.Where("type = ?", "A").Find(nil)
	base.Where("type = ?", "B").Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// derivedFromSessionUnsafe demonstrates that chain after Session is still mutable.
func derivedFromSessionUnsafe(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1)
	safe := base.Session(&gorm.Session{})
	derived := safe.Where("active = ?", true) // Mutable!

	derived.Find(nil)
	derived.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// multiLevelUnsafeDerivation demonstrates chained derivation.
func multiLevelUnsafeDerivation(db *gorm.DB) {
	level1 := db.Where("a")
	level2 := level1.Where("b")
	level3 := level2.Where("c")

	level3.Find(nil)
	level3.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Derived with Session
// =============================================================================

// derivedWithSession demonstrates safe derived variables with Session.
func derivedWithSession(db *gorm.DB) {
	queryDB := db.Model(&User{}).Where("name = ?", "jinzhu").Session(&gorm.Session{})
	q := queryDB.Where("age > ?", 10).Session(&gorm.Session{})
	q.Find(&[]User{})
	q.Count(new(int64)) // OK: Session at end
}

// sessionResetsClone demonstrates Session creating safe copy.
func sessionResetsClone(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1)
	safe := base.Session(&gorm.Session{})

	safe.Where("type = ?", "A").Find(nil)
	safe.Where("type = ?", "B").Find(nil) // OK: independent chains from safe
}

// withContextResetsClone demonstrates WithContext creating safe copy.
func withContextResetsClone(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1)
	safe := base.WithContext(nil)

	safe.Where("type = ?", "A").Find(nil)
	safe.Where("type = ?", "B").Find(nil) // OK: independent chains from safe
}

// sessionAtEachDerivation demonstrates Session at each derivation point.
func sessionAtEachDerivation(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1).Session(&gorm.Session{})
	derived := base.Where("active = ?", true).Session(&gorm.Session{})

	derived.Find(nil)
	derived.Count(nil) // OK: ends with Session
}

// =============================================================================
// SHOULD REPORT - Function return value
// =============================================================================

func helperWhere(db *gorm.DB, name string) *gorm.DB {
	return db.Model(&User{}).Where("name = ?", name)
}

// functionReturnValue demonstrates unsafe reuse of function return.
func functionReturnValue(db *gorm.DB) {
	q := helperWhere(db, "jinzhu")
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Function return with Session
// =============================================================================

// functionReturnWithSession demonstrates safe function return with Session.
func functionReturnWithSession(db *gorm.DB) {
	q := helperWhere(db, "jinzhu").Session(&gorm.Session{})
	q.Find(&[]User{})
	q.Count(new(int64)) // OK: Session at end
}

// =============================================================================
// SHOULD REPORT - Session on polluted value
// =============================================================================

// sessionAfterPolluted demonstrates that Session() after pollution doesn't help.
func sessionAfterPolluted(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true)
	q.Count(new(int64))                        // q is polluted
	q.Session(&gorm.Session{}).Find(&[]User{}) // want `\*gorm\.DB instance reused after chain method`
}

// sessionOnPollutedValue demonstrates Session on already-polluted value.
func sessionOnPollutedValue(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)
	q.Session(&gorm.Session{}).Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// multipleDirectUsesWithoutSession demonstrates multiple uses without Session.
func multipleDirectUsesWithoutSession(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)
	q.Count(nil)                          // want `\*gorm\.DB instance reused after chain method`
	q.Session(&gorm.Session{}).First(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Session before each finisher
// =============================================================================

// sessionBeforeEachFinisher demonstrates Session before each finisher.
func sessionBeforeEachFinisher(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true)

	q.Session(&gorm.Session{}).Count(new(int64))
	q.Session(&gorm.Session{}).Find(&[]User{}) // OK: Session before each use
}

// sessionBeforeFinisher demonstrates Session before each finisher (variant).
func sessionBeforeFinisher(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Session(&gorm.Session{}).Find(nil)
	q.Session(&gorm.Session{}).Count(nil) // OK: Session before each finisher
}

// =============================================================================
// SHOULD NOT REPORT - Reassignment
// =============================================================================

// reassignNewInstance demonstrates that reassigning a variable is safe.
func reassignNewInstance(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true)
	q.Count(new(int64))

	q = db.Where("name = ?", "test") // New instance assigned
	q.Find(&[]User{})                // OK
}

// =============================================================================
// SHOULD REPORT - Conditional chain extension (common pattern)
// =============================================================================

// conditionalExtendPartialWithPollution demonstrates conditional extension with initial pollution.
// One branch extends polluted q - that extension is a violation.
func conditionalExtendPartialWithPollution(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)
	q.Find(nil) // Pollutes q

	if flag {
		q = q.Where("y = ?", 2) // want `\*gorm\.DB instance reused after chain method`
	}

	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// conditionalExtendBothWithPollution demonstrates conditional extension in both branches with initial pollution.
// Both branches extend polluted q - both assignments are violations.
// After assignment, new roots are created and subsequent use is OK.
func conditionalExtendBothWithPollution(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)
	q.Find(nil) // Pollutes q

	if flag {
		q = q.Where("y = ?", 2) // want `\*gorm\.DB instance reused after chain method`
	} else {
		q = q.Where("z = ?", 3) // want `\*gorm\.DB instance reused after chain method`
	}

	q.Count(nil) // OK: both branches created new q via assignment
}

// conditionalExtendPartialNoPollution demonstrates conditional extension without initial pollution.
// Assignment in conditional creates new root - first use after is OK.
func conditionalExtendPartialNoPollution(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		q = q.Where("y = ?", 2) // Assignment creates new root from original q
	}

	q.Find(nil)                 // OK: first actual use
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// conditionalExtendBothNoPollution demonstrates conditional extension in both branches without initial pollution.
// Both branches create new roots via assignment - first use after is OK.
func conditionalExtendBothNoPollution(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)

	if flag {
		q = q.Where("y = ?", 2) // Assignment creates new root
	} else {
		q = q.Where("z = ?", 3) // Assignment creates new root
	}

	q.Find(nil)                 // OK: first actual use
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// conditionalExtendThreeBranches demonstrates three-way branch with partial assignment.
// Phi merges: original + two assignments. All paths have first use, so Find is OK.
func conditionalExtendThreeBranches(db *gorm.DB, n int) {
	q := db.Where("x = ?", 1) // Mutable root

	if n == 1 {
		q = q.Where("y = ?", 2) // Assignment creates new root
	} else if n == 2 {
		q = q.Where("z = ?", 3) // Assignment creates new root
	}
	// else: q remains unchanged (original mutable root)

	q.Find(nil)  // OK: Phi(q_1, q_2, q_3) - all first use
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// FIX GENERATION - Should NOT generate inappropriate fixes
// =============================================================================

// These test cases verify that fix generation is appropriate.
// The fix generator should only add reassignment (q = q.Where(...))
// when the top-level expression is a *gorm.DB non-finisher method call.

// nonFinisherOnlyGeneratesFix demonstrates that non-finisher expr statements
// get the reassignment fix (q = q.Where("x")).
func nonFinisherOnlyGeneratesFix(db *gorm.DB) {
	q := db.Where("base")
	q.Where("filter1") // Non-finisher expr stmt - should get "q = q.Where("filter1")" fix
	q.Find(nil)        // want `\*gorm\.DB instance reused after chain method`
}

// finisherDoesNotGenerateReassignmentFix demonstrates that finisher calls
// do NOT get reassignment fix (would be wrong to do "q = q.Find(nil)").
func finisherDoesNotGenerateReassignmentFix(db *gorm.DB) {
	q := db.Where("base")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// CLOSURE DEDUPLICATION - Should report only ONCE per position
// =============================================================================

// parentScopeVariable demonstrates that violations from closures accessing
// parent scope variables should be reported only once, not duplicated.
// Previously, this would report the same violation multiple times
// (once from parent function, once from closure).
func parentScopeVariable(db *gorm.DB) {
	q := db.Where("outer")
	func() {
		q.Where("inner")
		q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
	}()
}

// nestedClosureFixDedup demonstrates nested closures should not
// cause duplicate diagnostics at the same position.
func nestedClosureFixDedup(db *gorm.DB) {
	q := db.Where("base")
	func() {
		func() {
			q.Where("deep")
			q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
		}()
	}()
}

// tripleNestedClosureDedup demonstrates deeply nested closures.
// Should report only one violation, not 3 (one per closure scope).
func tripleNestedClosureDedup(db *gorm.DB) {
	q := db.Where("base")
	func() {
		func() {
			func() {
				q.Where("level3")
				q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
			}()
		}()
	}()
}

// =============================================================================
// NESTED ARGUMENTS - q.Or(q.Where(), q.Where()) patterns
// =============================================================================

// nestedArgsFromMutableParent demonstrates reuse of mutable q in nested args.
// When q is mutable, using it multiple times in Or() arguments is a violation.
func nestedArgsFromMutableParent(db *gorm.DB) {
	q := db.Where("base") // q is mutable
	q.Or(
		q.Where("a"), // want `\*gorm\.DB instance reused after chain method`
		q.Where("b"), // want `\*gorm\.DB instance reused after chain method`
	).Find(nil)
}

// nestedArgsFromImmutableParent demonstrates safe nested args with immutable q.
// When q is immutable (ends with Session), multiple branches are safe.
func nestedArgsFromImmutableParent(db *gorm.DB) {
	q := db.Where("base").Session(&gorm.Session{}) // q is immutable
	q.Or(
		q.Where("a"),
		q.Where("b"),
	).Find(nil) // OK: q is immutable, can branch freely
}

// nestedArgsFromMutableThenReuse shows reuse after nested args.
func nestedArgsFromMutableThenReuse(db *gorm.DB) {
	q := db.Where("base") // q is mutable
	q.Or(
		q.Where("a"), // want `\*gorm\.DB instance reused after chain method`
		q.Where("b"), // want `\*gorm\.DB instance reused after chain method`
	).Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// nestedArgsSingleUse demonstrates that even single nested arg is a violation
// because q.Or(q.Where("a")) uses q twice: once for q.Where("a"), once for q.Or(...).
func nestedArgsSingleUse(db *gorm.DB) {
	q := db.Where("base")
	q.Or(
		q.Where("a"), // want `\*gorm\.DB instance reused after chain method`
	).Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// WRAPPED IN NON-GORM FUNCTIONS - require.NoError(t, tx.Create(...).Error)
// =============================================================================

// wrappedInRequireNoError demonstrates GORM calls wrapped in require.NoError.
// The fix generator should NOT generate inappropriate fixes like:
//
//	tx = require.NoError(t, tx.Create(...).Error)  // WRONG!
//
// Instead, no reassignment fix should be generated for these cases because
// the top-level expression is not a *gorm.DB method call.
func wrappedInRequireNoError(tx *gorm.DB, t require.TestingT) {
	require.NoError(t, tx.Create(nil).Error)
	require.NoError(t, tx.Create(nil).Error)
	require.NoError(t, tx.Create(nil).Error)
}

// wrappedInRequireNoErrorMixed demonstrates mixed usage patterns.
func wrappedInRequireNoErrorMixed(tx *gorm.DB, t require.TestingT) {
	require.NoError(t, tx.Create(nil).Error)
	tx.Create(nil)
}

// =============================================================================
// FUNCTION ARGUMENT PATTERNS - q.Where() passed to various function types
// =============================================================================

// Helper functions for testing argument passing patterns

// voidFunc accepts *gorm.DB and returns nothing.
// By default, functions are assumed to pollute their arguments.
func voidFunc(db *gorm.DB) {
}

// returnsDB accepts *gorm.DB and returns *gorm.DB.
// By default, assumed to pollute argument and return mutable result.
func returnsDB(db *gorm.DB) *gorm.DB {
	return db
}

// pureReturnsDB accepts *gorm.DB and returns *gorm.DB.
// Marked pure: does NOT pollute argument, but returns mutable result.
//
//gormreuse:pure
func pureReturnsDB(db *gorm.DB) *gorm.DB {
	return db
}

// immutableReturnReturnsDB accepts *gorm.DB and returns *gorm.DB.
// Marked immutable-return: may pollute argument, but returns immutable result.
//
//gormreuse:immutable-return
func immutableReturnReturnsDB(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

// pureImmutableReturnReturnsDB accepts *gorm.DB and returns *gorm.DB.
// Marked pure,immutable-return: does NOT pollute argument AND returns immutable result.
//
//gormreuse:pure,immutable-return
func pureImmutableReturnReturnsDB(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

// --- Test cases for passing q.Where() to void function ---

// passToVoidFunc demonstrates passing q.Where() to a void function.
// voidFunc is assumed to pollute its argument, so q is polluted after the call.
func passToVoidFunc(db *gorm.DB) {
	q := db.Where("base")
	voidFunc(q.Where("a"))
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// --- Test cases for passing q.Where() to function returning *gorm.DB ---

// passToReturnsDB demonstrates passing q.Where() to a function returning *gorm.DB.
// returnsDB is assumed to pollute its argument and return mutable result.
func passToReturnsDB(db *gorm.DB) {
	q := db.Where("base")
	result := returnsDB(q.Where("a"))
	q.Find(nil)      // want `\*gorm\.DB instance reused after chain method`
	result.Find(nil) // OK: first use of result
	result.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// --- Test cases for passing q.Where() to pure function ---

// passToPureReturnsDB demonstrates passing q.Where() to a pure function.
// pureReturnsDB does NOT pollute its argument, but q.Where("a") itself
// is a use of q (creates a branch), so q is polluted after this call.
// The result is mutable.
func passToPureReturnsDB(db *gorm.DB) {
	q := db.Where("base")
	result := pureReturnsDB(q.Where("a"))
	q.Find(nil)      // want `\*gorm\.DB instance reused after chain method`
	result.Find(nil) // OK: first use of result
	result.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// --- Test cases for passing q.Where() to immutable-return function ---

// passToImmutableReturnReturnsDB demonstrates passing q.Where() to immutable-return function.
// immutableReturnReturnsDB may pollute argument, but returns immutable result.
func passToImmutableReturnReturnsDB(db *gorm.DB) {
	q := db.Where("base")
	result := immutableReturnReturnsDB(q.Where("a"))
	q.Find(nil)      // want `\*gorm\.DB instance reused after chain method`
	result.Find(nil) // OK: result is immutable
	result.Find(nil) // OK: result is immutable, can reuse freely
}

// --- Test cases for passing q.Where() to pure,immutable-return function ---

// passToPureImmutableReturnReturnsDB demonstrates passing q.Where() to pure,immutable-return function.
// pureImmutableReturnReturnsDB does NOT pollute its argument AND returns immutable result.
// However, q.Where("a") itself is a use of q (creates a branch), so q is polluted.
func passToPureImmutableReturnReturnsDB(db *gorm.DB) {
	q := db.Where("base")
	result := pureImmutableReturnReturnsDB(q.Where("a"))
	q.Find(nil)      // want `\*gorm\.DB instance reused after chain method`
	result.Find(nil) // OK: result is immutable
	result.Find(nil) // OK: result is immutable, can reuse freely
}

// --- Multiple calls to same function ---

// multipleCallsToVoidFunc demonstrates multiple calls passing q.Where() to void function.
func multipleCallsToVoidFunc(db *gorm.DB) {
	q := db.Where("base")
	voidFunc(q.Where("a"))
	voidFunc(q.Where("b")) // want `\*gorm\.DB instance reused after chain method`
}

// multipleCallsToPureFunc demonstrates multiple calls passing q.Where() to pure function.
// Even though pure function doesn't pollute, q.Where() itself uses q each time.
func multipleCallsToPureFunc(db *gorm.DB) {
	q := db.Where("base")
	pureReturnsDB(q.Where("a"))
	pureReturnsDB(q.Where("b")) // want `\*gorm\.DB instance reused after chain method`
	q.Find(nil)                 // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// PURE FUNCTION EFFECT - Passing q directly (not q.Where())
// =============================================================================

// passQDirectlyToNonPure demonstrates passing q directly to non-pure function.
// Non-pure function pollutes its argument, so q is polluted after the call.
func passQDirectlyToNonPure(db *gorm.DB) {
	q := db.Where("base")
	voidFunc(q)
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// passQDirectlyToPure demonstrates passing q directly to pure function.
// Pure function does NOT pollute its argument, so q is NOT polluted after the call.
func passQDirectlyToPure(db *gorm.DB) {
	q := db.Where("base")
	pureReturnsDB(q)
	q.Find(nil) // OK: pure function doesn't pollute q
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// passQDirectlyToPureMultiple demonstrates multiple calls passing q to pure function.
// Pure function does NOT pollute, so q can be passed multiple times.
func passQDirectlyToPureMultiple(db *gorm.DB) {
	q := db.Where("base")
	pureReturnsDB(q)
	pureReturnsDB(q) // OK: pure function doesn't pollute q
	pureReturnsDB(q) // OK: still not polluted
	q.Find(nil)      // OK: first actual use of q
	q.Find(nil)      // want `\*gorm\.DB instance reused after chain method`
}

// passQDirectlyToMixedFunctions demonstrates passing q to both pure and non-pure functions.
func passQDirectlyToMixedFunctions(db *gorm.DB) {
	q := db.Where("base")
	pureReturnsDB(q) // OK: pure doesn't pollute
	voidFunc(q)      // Pollutes q
	q.Find(nil)      // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// FALSE POSITIVE TESTS - Consecutive conditional reassignment
// =============================================================================

// consecutiveIfReassignment demonstrates consecutive if statements with reassignment.
// The q = q.Order(...) line should NOT report diagnostic since all prior uses are assignments.
// However, q.Count(nil) IS a violation (second use after Find), so fix is generated on Order line.
func consecutiveIfReassignment(db *gorm.DB, a, b bool) {
	q := db.Where("base")

	if a {
		q = q.Where("a") // Assignment, not consumption
	}

	if b {
		q = q.Where("b") // Assignment, not consumption
	}

	q = q.Order("c") // Assignment - no diagnostic here (fix added for Count violation below)

	q.Find(nil)      // First actual use - OK
	q.Count(nil)     // want `\*gorm\.DB instance reused after chain method`
}

// consecutiveIfSwitchReassignment demonstrates consecutive if and switch with reassignment.
// No diagnostic on Order line; fix is generated for Count violation.
func consecutiveIfSwitchReassignment(db *gorm.DB, keyword string, status *int) {
	q := db.Where("base")

	if keyword != "" {
		q = q.Where("keyword = ?", keyword) // Assignment
	}

	if status != nil {
		switch *status {
		case 1:
			q = q.Where("status = ?", 1) // Assignment
		case 2:
			q = q.Where("status = ?", 2) // Assignment
		}
	}

	q = q.Order("created_at") // Assignment - no diagnostic (fix for Count below)

	q.Find(nil)      // First actual use - OK
	q.Count(nil)     // want `\*gorm\.DB instance reused after chain method`
}

// helperFunctionReassignment tests reassignment with helper function return value.
// r.query(ctx) style helper that returns *gorm.DB.
// No diagnostic on Order line; fix is generated for Count violation.
func helperFunctionReassignment(db *gorm.DB, a, b bool) {
	helper := func() *gorm.DB { return db.Where("base") }
	q := helper()

	if a {
		q = q.Where("a") // Assignment
	}

	if b {
		q = q.Where("b") // Assignment
	}

	q = q.Order("c") // Assignment - no diagnostic (fix for Count below)

	q.Find(nil)      // First actual use - OK
	q.Count(nil)     // want `\*gorm\.DB instance reused after chain method`
}

type repo struct {
	db *gorm.DB
}

func (r *repo) query() *gorm.DB {
	return r.db.Where("base")
}

// methodReceiverReassignment tests reassignment with method receiver helper.
// No diagnostic on Order line; fix is generated for Count violation.
func methodReceiverReassignment(r *repo, a, b bool) {
	q := r.query()

	if a {
		q = q.Where("a") // Assignment
	}

	if b {
		q = q.Where("b") // Assignment
	}

	q = q.Order("c") // Assignment - no diagnostic (fix for Count below)

	q.Find(nil)      // First actual use - OK
	q.Count(nil)     // want `\*gorm\.DB instance reused after chain method`
}

type repoImmutable struct {
	db *gorm.DB
}

//gormreuse:immutable-return
func (r *repoImmutable) query() *gorm.DB {
	return r.db.Session(&gorm.Session{})
}

// immutableReturnMethodReassignment tests reassignment with immutable-return method.
// No diagnostic on Order line; fix is generated for Count violation.
func immutableReturnMethodReassignment(r *repoImmutable, a, b bool) {
	q := r.query() // q_1 is immutable

	if a {
		q = q.Where("a") // q_2 is mutable (derived from Where)
	}
	// Phi(q_1 immutable, q_2 mutable)

	if b {
		q = q.Where("b") // Assignment
	}

	q = q.Order("c") // Assignment - no diagnostic (fix for Count below)

	q.Find(nil)      // First actual use - OK
	q.Count(nil)     // want `\*gorm\.DB instance reused after chain method`
}

// switchMultipleCasesReassignment tests switch with multiple cases reassigning q.
// Each case uses the same q (from Phi before switch) and assigns back.
// No diagnostic on Order line; fix is generated for Count violation.
func switchMultipleCasesReassignment(db *gorm.DB, keyword string, status *int) {
	q := db.Where("base")

	if keyword != "" {
		q = q.Where("keyword = ?", keyword)
	}

	if status != nil {
		switch *status {
		case 1:
			q = q.Where("status = ?", 1)
		case 2:
			q = q.Where("status = ?", 2)
		case 3:
			q = q.Where("status = ?", 3)
		// No default - some paths don't reassign q
		}
	}

	q = q.Order("c") // Assignment - no diagnostic (fix for Count below)

	q.Find(nil)      // First actual use - OK
	q.Count(nil)     // want `\*gorm\.DB instance reused after chain method`
}

// switchMultipleCasesChainedReassignment tests switch with chained Where calls.
// This was the original false positive case - no diagnostic on Order line.
// Fix is generated for Count violation.
func switchMultipleCasesChainedReassignment(db *gorm.DB, status *int) {
	q := db.Where("base")

	if status != nil {
		switch *status {
		case 1:
			q = q.Where("a = ?", 1).Where("b = ?", 2)
		case 2:
			q = q.Where("c = ?", 3).Where("d = ?", 4)
		}
	}

	q = q.Order("e") // Assignment - no diagnostic (fix for Count below)

	q.Find(nil)      // First actual use - OK
	q.Count(nil)     // want `\*gorm\.DB instance reused after chain method`
}

// exactUserPatternImmutableReturn reproduces the exact user pattern:
// - immutable-return helper function
// - consecutive if with switch containing chained Where calls
// - final unconditional Order assignment
func exactUserPatternImmutableReturn(r *repoImmutable, keyword string, status *int) {
	q := r.query() // immutable-return

	if keyword != "" {
		q = q.Where("keyword = ?", keyword)
	}

	if status != nil {
		switch *status {
		case 1:
			q = q.Where("status = ?", 1).Where("extra = ?", 2)
		case 2:
			q = q.Where("status = ?", 3).Where("extra = ?", 4)
		case 3:
			q = q.Where("status = ?", 5)
		}
	}

	q = q.Order("created_at") // Should NOT report diagnostic here

	q.Find(nil) // First actual use - OK
}
