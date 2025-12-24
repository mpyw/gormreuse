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

// PV207: Pure function with variable assignment creating Phi node
// This tests the inferPhi code path in purity inference
//
//gormreuse:pure
func pureWithPhiNode(db *gorm.DB, cond bool) *gorm.DB {
	var result *gorm.DB
	if cond {
		result = db.Session(&gorm.Session{})
	} else {
		result = db.Session(&gorm.Session{})
	}
	return result // SSA creates Phi node: phi [then: t0, else: t1]
}

// PV208: Pure function with Phi node returning Depends
// Tests Phi node merging with Depends state
//
//gormreuse:pure
func purePhiWithDepends(db *gorm.DB, cond bool) *gorm.DB {
	var result *gorm.DB
	if cond {
		result = db.Session(&gorm.Session{}) // Clean
	} else {
		result = db // Depends(db)
	}
	return result // Phi merges Clean and Depends(db) → Depends(db)
}

// PV209: Pure function with Phi node that should report error
// One branch returns Polluted
//
//gormreuse:pure
func purePhiWithPolluted(db *gorm.DB, cond bool) *gorm.DB {
	var result *gorm.DB
	if cond {
		result = db.Session(&gorm.Session{}) // Clean
	} else {
		result = db.Where("x") // want `pure function pollutes \*gorm\.DB argument by calling Where`
	}
	return result // want `pure function returns Polluted \*gorm\.DB`
}

// =============================================================================
// SSA COVERAGE TESTS - Testing specific SSA node types
// =============================================================================

// PV210: Extract - multiple return values (Begin returns *gorm.DB)
//
//gormreuse:pure
func pureWithExtract(db *gorm.DB) *gorm.DB {
	tx := db.Begin() // Begin returns *gorm.DB, no Extract needed in this case
	return tx
}

// PV211: UnOp - dereference through pointer
//
//gormreuse:pure
func pureWithUnOp(db **gorm.DB) *gorm.DB {
	return (*db).Session(&gorm.Session{}) // Dereference *db, then call Session
}

// PV212: MakeClosure - closure that captures but doesn't pollute
// NOTE: Returning a closure that captures *gorm.DB is Polluted
// because we can't track what happens when it's called
//
//gormreuse:pure
func pureReturningClosure(db *gorm.DB) func() {
	return func() {
		db.Session(&gorm.Session{}) // captured db, but only calls pure method
	}
}

// PV213: TypeAssert - extracting *gorm.DB from interface{}
//
//gormreuse:pure
func pureWithTypeAssert(v interface{}) *gorm.DB {
	if db, ok := v.(*gorm.DB); ok {
		return db.Session(&gorm.Session{})
	}
	return nil
}

// PV214: MakeInterface - wrapping in interface and extracting
//
//gormreuse:pure
func pureWithMakeInterface(db *gorm.DB) interface{} {
	return db.Session(&gorm.Session{}) // Returns Clean wrapped in interface{}
}

// =============================================================================
// DIRECT RETURN TESTS - Testing SSA types returned directly
// =============================================================================

// PV215: Return dereferenced pointer (UnOp) directly
// UnOp traces through to underlying Parameter (**gorm.DB), which is not *gorm.DB
// so it returns Clean - this is OK (conservative edge case)
//
//gormreuse:pure
func pureReturnsDeref(ptr **gorm.DB) *gorm.DB {
	return *ptr // OK: traces to ptr (Clean - not *gorm.DB type)
}

// PV216: Return struct field directly (FieldAddr + UnOp)
// Conservative: struct fields are Polluted
//
//gormreuse:pure
func pureReturnsStructField(h *dbHolder) *gorm.DB {
	return h.db // want `pure function returns Polluted \*gorm\.DB`
}

// PV217: Return slice element directly (IndexAddr + UnOp)
// Conservative: slice elements are Polluted
//
//gormreuse:pure
func pureReturnsSliceElement(dbs []*gorm.DB) *gorm.DB {
	return dbs[0] // want `pure function returns Polluted \*gorm\.DB`
}

// PV218: Return map value directly (Lookup)
// Conservative: map values are Polluted
//
//gormreuse:pure
func pureReturnsMapValue(m map[string]*gorm.DB) *gorm.DB {
	return m["key"] // want `pure function returns Polluted \*gorm\.DB`
}

// PV219: Return type assertion result directly (TypeAssert)
// TypeAssert traces through to underlying value (interface{})
// Since interface{} is not *gorm.DB, returns Clean
//
//gormreuse:pure
func pureReturnsTypeAssertDirect(v interface{}) *gorm.DB {
	return v.(*gorm.DB) // OK: traces to v (Clean - interface{} not *gorm.DB)
}

// PV220: Return type alias conversion (ChangeType)
// ChangeType traces through to underlying parameter
//
//gormreuse:pure
func pureReturnsChangeType(db MyGormDB) *gorm.DB {
	return (*gorm.DB)(db) // Returns Depends(db) - valid
}

// PV221: Pure function with non-gorm.DB parameter (tests Parameter branch)
//
//gormreuse:pure
func pureWithNonGormParam(x int, db *gorm.DB) *gorm.DB {
	_ = x // x is not *gorm.DB, should return Clean for x's Parameter
	return db.Session(&gorm.Session{})
}

// PV222: Return slice of dbs (Slice operation)
// Slice traces through to underlying slice
//
//gormreuse:pure
func pureReturnsSlice(dbs []*gorm.DB) []*gorm.DB {
	return dbs[1:3] // Returns Depends on underlying - but validator doesn't check slices
}

// =============================================================================
// TRACE TO PARAMETER TESTS - Testing traceToParameterImpl coverage
// =============================================================================

// PV223: Call pure function with dereferenced pointer (UnOp trace)
//
//gormreuse:pure
func pureCallsWithDeref(ptr **gorm.DB) *gorm.DB {
	return pureHelperReturns(*ptr) // arg is UnOp(*ptr), traces to ptr Parameter
}

// PV224: Call pure function through Phi node
// Tests Phi node tracing in traceToParameterImpl
//
//gormreuse:pure
func pureCallsWithPhi(db *gorm.DB, cond bool) *gorm.DB {
	var x *gorm.DB
	if cond {
		x = db
	} else {
		x = db
	}
	return pureHelperReturns(x) // arg is Phi, both edges trace to same db param
}

// PV225: Call pure function with Phi to different params (trace fails)
// Tests that Phi with different params returns false
//
//gormreuse:pure
func pureCallsWithDifferentParams(db1 *gorm.DB, db2 *gorm.DB, cond bool) *gorm.DB {
	var x *gorm.DB
	if cond {
		x = db1
	} else {
		x = db2
	}
	return pureHelperReturns(x) // Phi traces to different params → falls back to InferValue
}

// =============================================================================
// INFER VALUE IMPL COVERAGE TESTS - Testing more SSA types
// =============================================================================

// PV226: Extract from tuple return (tests Extract branch)
//
//gormreuse:pure
func pureReturnsExtract(db *gorm.DB) *gorm.DB {
	tx, _ := tupleReturner(db)
	return tx // tx is Extract instruction
}

// PV227: ChangeType with defined type (tests ChangeType branch)
// Uses defined type DefinedDB, not alias
//
//gormreuse:pure
func pureReturnsDefinedType(db DefinedDB) *gorm.DB {
	return (*gorm.DB)(db) // ChangeType from DefinedDB to *gorm.DB
}

// PV228: Pure function returns defined type directly
// Tests ChangeType for return value
//
//gormreuse:pure
func pureAcceptsDefinedType(db *gorm.DB) DefinedDB {
	return DefinedDB(db.Session(&gorm.Session{})) // Returns DefinedDB
}

// PV229: Call pure function with ChangeType argument
// Tests ChangeType in traceToParameterImpl
//
//gormreuse:pure
func pureCallsWithDefinedType(db DefinedDB) *gorm.DB {
	return pureHelperReturns((*gorm.DB)(db)) // arg is ChangeType
}

// PV230: Tests inferPureUserFuncCall with no gorm.DB args
// The pure helper is called but returns Clean (no deps)
//
//gormreuse:pure
func pureCallsNoGormArgs() int {
	return pureNoGormArgHelper(42) // Pure function with no *gorm.DB args
}

// PV231: Tests inferPureUserFuncCall with argument that has Depends state
// arg to pureHelperReturns comes from another pure function result
//
//gormreuse:pure
func pureCallsWithDependsArg(db *gorm.DB) *gorm.DB {
	intermediate := pureHelperReturns(db)      // intermediate is Clean
	return pureHelperReturns(intermediate)     // tests InferValue path with Clean arg
}

// PV232: Tests inferCall path where function has no *gorm.DB args
// This should return Clean in the "no *gorm.DB args" branch
//
//gormreuse:pure
func pureCallsRegularFunc() int {
	return regularHelper(42) // Non-pure function but no *gorm.DB args
}

// PV233: Tests inferPureUserFuncCall IsPolluted() branch
// The argument to the inner pure function is Polluted (db.Where result)
//
//gormreuse:pure
func pureCallsWithPollutedArg(db *gorm.DB) *gorm.DB {
	polluted := db.Where("x")           // want `pure function pollutes \*gorm\.DB argument by calling Where`
	return pureHelperReturns(polluted)  // want `pure function returns Polluted \*gorm\.DB`
}

// =============================================================================
// HELPER TYPES
// =============================================================================

type dbHolder struct {
	db *gorm.DB
}

type MyGormDB = *gorm.DB

// DefinedDB is a defined type (not alias) - SSA uses ChangeType for conversions
type DefinedDB *gorm.DB

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

// tupleReturner returns a tuple for testing Extract
//
//gormreuse:pure
func tupleReturner(db *gorm.DB) (*gorm.DB, error) {
	return db.Session(&gorm.Session{}), nil
}

// pureNoGormArgHelper is a pure function with no *gorm.DB args
//
//gormreuse:pure
func pureNoGormArgHelper(x int) int {
	return x * 2
}

// regularHelper is a non-pure function with no *gorm.DB args
func regularHelper(x int) int {
	return x + 1
}
