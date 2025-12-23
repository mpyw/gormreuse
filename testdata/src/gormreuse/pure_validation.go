package internal

import "gorm.io/gorm"

// =============================================================================
// Pure Function Validation Test Cases
//
// This file tests validation of //gormreuse:pure directive.
// Pure functions must:
//   1. NOT pollute *gorm.DB arguments (no chain method calls on them)
//   2. Return immutable *gorm.DB if returning one (Session/WithContext/etc. result)
//
// Test Matrix Variables:
//   - Has *gorm.DB argument: yes/no
//   - Returns *gorm.DB: yes/no
//   - What's done with argument: nothing / pure builtin / non-pure / pass to pure func / pass to non-pure func
//   - What's returned: arg directly / pure builtin result / non-pure result / pure func result / non-pure func result
// =============================================================================

// =============================================================================
// SHOULD REPORT - Pure function pollutes argument
// =============================================================================

// PV001: Pure function calls non-pure method on argument
//
//gormreuse:pure
func purePollutesByWhere(db *gorm.DB) {
	db.Where("x = ?", 1) // want `pure function pollutes \*gorm\.DB argument by calling Where`
}

// PV002: Pure function calls Find (terminal) on argument
//
//gormreuse:pure
func purePollutesByFind(db *gorm.DB) {
	db.Find(nil) // want `pure function pollutes \*gorm\.DB argument by calling Find`
}

// PV003: Pure function calls non-pure chain on argument
//
//gormreuse:pure
func purePollutesByChain(db *gorm.DB) {
	db.Where("x").Where("y").Find(nil) // want `pure function pollutes \*gorm\.DB argument by calling Where` `pure function pollutes \*gorm\.DB argument by calling Where` `pure function pollutes \*gorm\.DB argument by calling Find`
}

// PV004: Pure function passes argument to non-pure function
//
//gormreuse:pure
func purePollutesViaNonPureFunc(db *gorm.DB) {
	nonPureHelper(db) // want `pure function passes \*gorm\.DB argument to non-pure function nonPureHelper`
}

// PV005: Pure function returns argument directly - NOW VALID with 3-state model!
// The return state is Depends(db), which is valid for pure functions.
//
//gormreuse:pure
func pureReturnsArgDirectly(db *gorm.DB) *gorm.DB {
	return db // OK: returns Depends(db), purity depends on caller's argument
}

// PV006: Pure function returns non-pure method result on argument
//
//gormreuse:pure
func pureReturnsWhereResult(db *gorm.DB) *gorm.DB {
	return db.Where("x") // want `pure function pollutes \*gorm\.DB argument by calling Where` `pure function returns Polluted \*gorm\.DB`
}

// PV007: Pure function returns non-pure function result
//
//gormreuse:pure
func pureReturnsNonPureFuncResult(db *gorm.DB) *gorm.DB {
	return nonPureHelperReturns(db) // want `pure function passes \*gorm\.DB argument to non-pure function nonPureHelperReturns` `pure function returns Polluted \*gorm\.DB`
}

// PV008: Pure function with multiple DB args, pollutes one
//
//gormreuse:pure
func purePollutesOneOfMany(db1 *gorm.DB, db2 *gorm.DB) {
	db1.Session(&gorm.Session{}) // OK: pure method
	db2.Where("x")               // want `pure function pollutes \*gorm\.DB argument by calling Where`
}

// PV009: Pure function operates on Session result - this is OK
// Session returns immutable, so calling Where on it doesn't pollute the argument
//
//gormreuse:pure
func pureOperatesOnSessionResult(db *gorm.DB) {
	s := db.Session(&gorm.Session{})
	s.Where("x") // OK: s is immutable (result of Session), so this doesn't pollute db
}

// =============================================================================
// SHOULD NOT REPORT - Valid pure functions
// =============================================================================

// PV101: Pure function does nothing with argument
//
//gormreuse:pure
func pureDoesNothing(db *gorm.DB) {
	_ = db // Just reference, no method calls
}

// PV102: Pure function only calls pure builtin methods
//
//gormreuse:pure
func pureOnlyCallsSession(db *gorm.DB) {
	db.Session(&gorm.Session{}) // OK: pure method
}

// PV103: Pure function only calls WithContext
//
//gormreuse:pure
func pureOnlyCallsWithContext(db *gorm.DB) {
	db.WithContext(nil) // OK: pure method
}

// PV104: Pure function only calls Debug
//
//gormreuse:pure
func pureOnlyCallsDebug(db *gorm.DB) {
	db.Debug() // OK: pure method
}

// PV105: Pure function passes argument to another pure function
//
//gormreuse:pure
func purePassesToPure(db *gorm.DB) {
	pureDoesNothing(db) // OK: passing to pure function
}

// PV106: Pure function returns Session result
//
//gormreuse:pure
func pureReturnsSessionResult(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{}) // OK: returns immutable
}

// PV107: Pure function returns WithContext result
//
//gormreuse:pure
func pureReturnsWithContextResult(db *gorm.DB) *gorm.DB {
	return db.WithContext(nil) // OK: returns immutable
}

// PV108: Pure function returns another pure function result
//
//gormreuse:pure
func pureReturnsPureFuncResult(db *gorm.DB) *gorm.DB {
	return pureReturnsSessionResult(db) // OK: returns result of pure function
}

// PV109: Pure function without *gorm.DB argument
//
//gormreuse:pure
func pureNoGormArg(x int) int {
	return x * 2 // OK: no *gorm.DB involved
}

// PV110: Pure function without *gorm.DB return
//
//gormreuse:pure
func pureNoGormReturn(db *gorm.DB) int {
	db.Session(&gorm.Session{}) // OK: pure method, result discarded
	return 42
}

// PV111: Pure function chains pure methods and returns
//
//gormreuse:pure
func pureChainsPure(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{}).WithContext(nil) // OK: both pure
}

// PV112: Pure function with conditional return (both branches pure)
//
//gormreuse:pure
func pureConditionalBothPure(db *gorm.DB, cond bool) *gorm.DB {
	if cond {
		return db.Session(&gorm.Session{})
	}
	return db.WithContext(nil)
}

// PV113: Pure function uses Begin (creates new transaction)
//
//gormreuse:pure
func pureUsesBegin(db *gorm.DB) *gorm.DB {
	tx := db.Begin()
	return tx // OK: Begin returns immutable
}

// PV114: Pure function with multiple DB args, all handled safely
//
//gormreuse:pure
func pureSafeWithMultipleArgs(db1 *gorm.DB, db2 *gorm.DB) *gorm.DB {
	db1.Session(&gorm.Session{}) // OK: pure method
	return db2.WithContext(nil)  // OK: returns immutable
}

// =============================================================================
// EDGE CASES - Combinations and boundary conditions
// =============================================================================

// PV201: Pure function with nested call (pure calling pure)
//
//gormreuse:pure
func pureNestedPure(db *gorm.DB) *gorm.DB {
	return pureReturnsSessionResult(pureReturnsSessionResult(db))
}

// PV202: Pure function with nil return (no *gorm.DB)
//
//gormreuse:pure
func pureReturnsNil(db *gorm.DB) *gorm.DB {
	db.Session(&gorm.Session{})
	return nil // OK: nil is immutable
}

// PV203: Pure function that only uses db in condition
//
//gormreuse:pure
func pureOnlyInCondition(db *gorm.DB) *gorm.DB {
	if db != nil {
		return db.Session(&gorm.Session{})
	}
	return nil
}

// PV204: Pure function with local variable assignment
//
//gormreuse:pure
func pureWithLocalVar(db *gorm.DB) *gorm.DB {
	safe := db.Session(&gorm.Session{})
	return safe // OK: safe is immutable
}

// PV205: Pure function pollutes after checking condition
//
//gormreuse:pure
func purePollutesConditionally(db *gorm.DB, cond bool) {
	if cond {
		db.Where("x") // want `pure function pollutes \*gorm\.DB argument by calling Where`
	}
}

// PV206: Pure function with mixed return paths - NOW VALID with 3-state model!
// Both branches return valid states:
//   - if cond: Session() returns Clean
//   - else: return db returns Depends(db)
// Merged state: Depends(db) - valid for pure function
//
//gormreuse:pure
func pureMixedReturnPaths(db *gorm.DB, cond bool) *gorm.DB {
	if cond {
		return db.Session(&gorm.Session{}) // Clean
	}
	return db // Depends(db) - valid!
}

// =============================================================================
// HELPER FUNCTIONS (not marked as pure)
// =============================================================================

func nonPureHelper(db *gorm.DB) {
	db.Where("x = ?", 1).Find(nil)
}

func nonPureHelperReturns(db *gorm.DB) *gorm.DB {
	return db.Where("x = ?", 1)
}

//gormreuse:pure
func pureHelperReturns(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}
