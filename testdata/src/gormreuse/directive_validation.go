package internal

import "gorm.io/gorm"

// =============================================================================
// Directive Validation Test Cases
//
// This file tests validation and behavior of gormreuse directives:
//
//   //gormreuse:pure             - Function doesn't pollute *gorm.DB arguments
//   //gormreuse:immutable-return - Function returns immutable *gorm.DB (like Session)
//   //gormreuse:pure,immutable-return - Both guarantees combined
//
// Test Sections:
//   1. Pure function validation (PV prefix) - tests pure contract enforcement
//   2. Immutable-return function behavior (IR prefix) - tests return value treated as immutable
//   3. Combined pure,immutable-return (PIR prefix) - tests both guarantees
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
// Note: The "returns Polluted" is no longer an error - pure only guarantees no argument pollution.
//
//gormreuse:pure
func pureReturnsWhereResult(db *gorm.DB) *gorm.DB {
	return db.Where("x") // want `pure function pollutes \*gorm\.DB argument by calling Where`
}

// PV007: Pure function returns non-pure function result
// Note: The "returns Polluted" is no longer an error - pure only guarantees no argument pollution.
//
//gormreuse:pure
func pureReturnsNonPureFuncResult(db *gorm.DB) *gorm.DB {
	return nonPureHelperReturns(db) // want `pure function passes \*gorm\.DB argument to non-pure function nonPureHelperReturns`
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

// PV111b: Pure function chains Session then non-pure (direct chain, no assignment)
// Session() returns immutable, so Where() on it doesn't pollute db.
//
//gormreuse:pure
func pureSessionThenWhereDirect(db *gorm.DB) {
	db.Session(&gorm.Session{}).Where("x") // OK: Where on immutable doesn't pollute db
}

// PV111c: Pure function returns Session().Where() chain (README example pattern)
// This is safe because Session() returns immutable, Where() operates on that.
//
//gormreuse:pure
func pureReturnsSessionWhereChain(db *gorm.DB, tenantID int) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID) // OK
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
// One branch pollutes the argument (the return being Polluted is no longer an error).
//
//gormreuse:pure
func purePhiWithPolluted(db *gorm.DB, cond bool) *gorm.DB {
	var result *gorm.DB
	if cond {
		result = db.Session(&gorm.Session{}) // Clean
	} else {
		result = db.Where("x") // want `pure function pollutes \*gorm\.DB argument by calling Where`
	}
	return result // OK now - pure doesn't guarantee immutable return
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
// No argument pollution - OK (pure doesn't guarantee immutable return).
//
//gormreuse:pure
func pureReturnsStructField(h *dbHolder) *gorm.DB {
	return h.db // OK - no *gorm.DB argument pollution
}

// PV217: Return slice element directly (IndexAddr + UnOp)
// No argument pollution - OK (pure doesn't guarantee immutable return).
//
//gormreuse:pure
func pureReturnsSliceElement(dbs []*gorm.DB) *gorm.DB {
	return dbs[0] // OK - no *gorm.DB argument pollution
}

// PV218: Return map value directly (Lookup)
// No argument pollution - OK (pure doesn't guarantee immutable return).
//
//gormreuse:pure
func pureReturnsMapValue(m map[string]*gorm.DB) *gorm.DB {
	return m["key"] // OK - no *gorm.DB argument pollution
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
// Note: "returns Polluted" is no longer an error - pure only checks argument pollution.
//
//gormreuse:pure
func pureCallsWithPollutedArg(db *gorm.DB) *gorm.DB {
	polluted := db.Where("x")           // want `pure function pollutes \*gorm\.DB argument by calling Where`
	return pureHelperReturns(polluted)  // OK now - return value purity not checked
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

// #############################################################################
// #############################################################################
// ##                                                                         ##
// ##              IMMUTABLE-RETURN DIRECTIVE TEST CASES                      ##
// ##                                                                         ##
// #############################################################################
// #############################################################################

// =============================================================================
// SHOULD NOT REPORT - immutable-return function returns are treated as immutable
//
// The //gormreuse:immutable-return directive marks a function as returning
// immutable *gorm.DB, similar to builtin methods like Session() and WithContext().
// Callers can safely reuse the return value without Session() wrapping.
// =============================================================================

// IR001: Basic immutable-return function - return value can be reused
//
//gormreuse:immutable-return
func getImmutableDB() *gorm.DB {
	return globalDB.Session(&gorm.Session{})
}

func useImmutableReturn() {
	db := getImmutableDB()
	db.Where("x").Find(nil)
	db.Where("y").Find(nil) // OK: getImmutableDB returns immutable
}

// IR002: immutable-return with Session - common pattern for DB connection helpers
//
//gormreuse:immutable-return
func getDBWithSession() *gorm.DB {
	return globalDB.Session(&gorm.Session{})
}

func useDBWithSession() {
	db := getDBWithSession()
	db.Where("a").Find(nil)
	db.Where("b").Find(nil) // OK: getDBWithSession returns immutable
}

// IR003: immutable-return method on struct - repository pattern
type IRRepository struct {
	db *gorm.DB
}

//gormreuse:immutable-return
func (r *IRRepository) DB() *gorm.DB {
	return r.db.Session(&gorm.Session{})
}

func useRepositoryDB(r *IRRepository) {
	db := r.DB()
	db.Where("x").Find(nil)
	db.Where("y").Find(nil) // OK: IRRepository.DB returns immutable
}

// IR004: immutable-return used in conditional branches
//
//gormreuse:immutable-return
func getDBForTenant(tenantID int) *gorm.DB {
	return globalDB.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID)
}

func useDBInBranches(cond bool) {
	db := getDBForTenant(1)
	if cond {
		db.Where("x").Find(nil)
	} else {
		db.Where("y").Find(nil)
	}
	db.Count(nil) // OK: getDBForTenant returns immutable
}

// IR005: immutable-return in loop - each iteration gets fresh immutable
//
//gormreuse:immutable-return
func getFreshDB() *gorm.DB {
	return globalDB.Session(&gorm.Session{})
}

func useInLoop(items []int) {
	for _, item := range items {
		db := getFreshDB()
		db.Where("item = ?", item).Find(nil)
		db.Count(nil) // OK: each iteration gets fresh immutable
	}
}

// IR006: immutable-return chained with other calls
func useImmutableChained() {
	getImmutableDB().Where("x").Find(nil)
	getImmutableDB().Where("y").Find(nil) // OK: each call returns fresh immutable
}

// IR007: immutable-return assigned to multiple variables
func useMultipleAssignments() {
	db1 := getImmutableDB()
	db2 := getImmutableDB()
	db1.Where("x").Find(nil)
	db2.Where("y").Find(nil)
	db1.Count(nil) // OK: db1 is immutable
	db2.Count(nil) // OK: db2 is immutable
}

// =============================================================================
// SHOULD REPORT - immutable-return doesn't prevent reuse of mutable intermediates
//
// The immutable-return directive only affects the RETURN VALUE of the function.
// If you create mutable intermediates inside the function and pass them around,
// normal reuse rules still apply.
// =============================================================================

// IR101: Using immutable-return result, then deriving mutable from it
func deriveMutableFromImmutable() {
	db := getImmutableDB() // immutable
	q := db.Where("x")     // q is mutable (derived from chain method)
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// IR102: Mixing immutable-return with regular chain methods
func mixImmutableAndMutable(db *gorm.DB) {
	imm := getImmutableDB()
	q := db.Where("x") // q is mutable
	imm.Find(nil)      // OK: imm is immutable
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// #############################################################################
// #############################################################################
// ##                                                                         ##
// ##        COMBINED PURE + IMMUTABLE-RETURN DIRECTIVE TEST CASES            ##
// ##                                                                         ##
// #############################################################################
// #############################################################################

// =============================================================================
// SHOULD NOT REPORT - pure,immutable-return provides both guarantees
//
// //gormreuse:pure,immutable-return means:
//   1. The function doesn't pollute *gorm.DB arguments (pure)
//   2. The function returns immutable *gorm.DB (immutable-return)
//
// This is the recommended pattern for DB connection helpers that take a context
// or other parameters and return a configured *gorm.DB.
// =============================================================================

// PIR001: Basic pure,immutable-return - the ideal DB helper pattern
//
//gormreuse:pure,immutable-return
func getDB() *gorm.DB {
	return globalDB.Session(&gorm.Session{})
}

func usePureImmutableReturn() {
	db := getDB()
	db.Where("x").Find(nil)
	db.Where("y").Find(nil) // OK: getDB returns immutable
}

// PIR002: pure,immutable-return with *gorm.DB argument - wraps existing DB
//
//gormreuse:pure,immutable-return
func wrapDB(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

func useWrapDB(db *gorm.DB) {
	wrapped := wrapDB(db)
	wrapped.Where("x").Find(nil)
	wrapped.Where("y").Find(nil) // OK: wrapDB returns immutable
	db.Find(nil)                 // OK: db not polluted by wrapDB (pure)
}

// PIR003: pure,immutable-return with multiple DB arguments
//
//gormreuse:pure,immutable-return
func selectDB(db1 *gorm.DB, db2 *gorm.DB, useFirst bool) *gorm.DB {
	if useFirst {
		return db1.Session(&gorm.Session{})
	}
	return db2.Session(&gorm.Session{})
}

func useSelectDB(db1 *gorm.DB, db2 *gorm.DB) {
	selected := selectDB(db1, db2, true)
	selected.Where("x").Find(nil)
	selected.Where("y").Find(nil) // OK: selectDB returns immutable
	db1.Find(nil)                 // OK: db1 not polluted
	db2.Find(nil)                 // OK: db2 not polluted
}

// PIR004: pure,immutable-return tenant isolation pattern
//
//gormreuse:pure,immutable-return
func getTenantDB(db *gorm.DB, tenantID int) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID)
}

func useTenantDB(db *gorm.DB) {
	tenant1 := getTenantDB(db, 1)
	tenant2 := getTenantDB(db, 2)
	tenant1.Where("x").Find(nil)
	tenant2.Where("y").Find(nil)
	tenant1.Count(nil) // OK: getTenantDB returns immutable
	tenant2.Count(nil) // OK: getTenantDB returns immutable
	db.Find(nil)       // OK: db not polluted
}

// PIR005: pure,immutable-return with method receiver
type DBFactory struct {
	baseDB *gorm.DB
}

//gormreuse:pure,immutable-return
func (f *DBFactory) NewDB() *gorm.DB {
	return f.baseDB.Session(&gorm.Session{})
}

func useDBFactory(f *DBFactory) {
	db1 := f.NewDB()
	db2 := f.NewDB()
	db1.Where("x").Find(nil)
	db2.Where("y").Find(nil)
	db1.Count(nil) // OK: NewDB returns immutable
	db2.Count(nil) // OK: NewDB returns immutable
}

// =============================================================================
// SHOULD REPORT - pure,immutable-return still validates pure contract
//
// Even with immutable-return, the pure contract is still enforced.
// If the function pollutes its *gorm.DB argument, it will be reported.
// =============================================================================

// PIR101: pure,immutable-return but pollutes argument
//
//gormreuse:pure,immutable-return
func badPureImmutable(db *gorm.DB) *gorm.DB {
	db.Where("x") // want `pure function pollutes \*gorm\.DB argument by calling Where`
	return db.Session(&gorm.Session{})
}

// PIR102: pure,immutable-return but passes to non-pure function
//
//gormreuse:pure,immutable-return
func badPureImmutablePassesNonPure(db *gorm.DB) *gorm.DB {
	nonPureHelper(db) // want `pure function passes \*gorm\.DB argument to non-pure function nonPureHelper`
	return db.Session(&gorm.Session{})
}

// =============================================================================
// EDGE CASES - Directive combinations and special scenarios
// =============================================================================

// PIR201: Only immutable-return (no pure) - can pollute argument
// This function pollutes its argument but that's allowed without pure directive.
// The return value is still treated as immutable.
//
//gormreuse:immutable-return
func immutableOnlyPollutes(db *gorm.DB) *gorm.DB {
	db.Where("pollute") // No error - not marked as pure
	return db.Session(&gorm.Session{})
}

func useImmutableOnlyPollutes(db *gorm.DB) {
	result := immutableOnlyPollutes(db)
	result.Where("x").Find(nil)
	result.Where("y").Find(nil) // OK: return is immutable
	// Note: db is polluted by immutableOnlyPollutes, but we don't track that
}

// PIR202: immutable-return called multiple times in sequence
func multipleImmutableCalls() {
	db1 := getImmutableDB()
	db1.Where("a").Find(nil)

	db2 := getImmutableDB()
	db2.Where("b").Find(nil)

	db1.Count(nil) // OK: db1 is from immutable-return
	db2.Count(nil) // OK: db2 is from immutable-return
}

// PIR203: Nested pure,immutable-return calls
//
//gormreuse:pure,immutable-return
func outerPureImmutable(db *gorm.DB) *gorm.DB {
	return wrapDB(db) // wrapDB is also pure,immutable-return
}

func useNestedPureImmutable(db *gorm.DB) {
	result := outerPureImmutable(db)
	result.Where("x").Find(nil)
	result.Where("y").Find(nil) // OK: returns immutable
	db.Find(nil)                // OK: db not polluted
}

// =============================================================================
// MULTI-LINE DIRECTIVE COMBINATIONS (FuncDecl)
//
// Tests for directives on separate lines for named functions.
// =============================================================================

// PIR301: pure and immutable-return on separate lines (FuncDecl)
//
//gormreuse:pure
//gormreuse:immutable-return
func multiLinePureImmutable(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

func useMultiLinePureImmutable(db *gorm.DB) {
	result := multiLinePureImmutable(db)
	result.Where("x").Find(nil)
	result.Where("y").Find(nil) // OK: returns immutable
	db.Find(nil)                // OK: db not polluted (pure)
}

// PIR302: pure and immutable-return with unrelated comment in between (FuncDecl)
//
//gormreuse:pure
// This is an unrelated comment
//gormreuse:immutable-return
func multiLinePureImmutableWithComment(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

func useMultiLinePureImmutableWithComment(db *gorm.DB) {
	result := multiLinePureImmutableWithComment(db)
	result.Where("x").Find(nil)
	result.Where("y").Find(nil) // OK: returns immutable
	db.Find(nil)                // OK: db not polluted (pure)
}

// PIR303: directives with trailing comments (FuncDecl)
//
//gormreuse:pure // marks as pure
//gormreuse:immutable-return // marks return as immutable
func multiLineWithTrailing(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

func useMultiLineWithTrailing(db *gorm.DB) {
	result := multiLineWithTrailing(db)
	result.Where("x").Find(nil)
	result.Where("y").Find(nil) // OK: returns immutable
	db.Find(nil)                // OK: db not polluted (pure)
}

// =============================================================================
// SAME-LINE DIRECTIVE (FuncDecl)
//
// Tests for directives after opening brace on same line for named functions.
// =============================================================================

// PIR401: pure directive after opening brace (FuncDecl)
func sameLinePure(db *gorm.DB) { //gormreuse:pure
	_ = db
}

func useSameLinePure(db *gorm.DB) {
	q := db.Where("x")
	sameLinePure(q)
	q.Find(nil) // OK: sameLinePure is pure
}

// PIR402: immutable-return directive after opening brace (FuncDecl)
func sameLineImmutableReturn(db *gorm.DB) *gorm.DB { //gormreuse:immutable-return
	return db.Session(&gorm.Session{})
}

func useSameLineImmutableReturn(db *gorm.DB) {
	result := sameLineImmutableReturn(db)
	result.Where("x").Find(nil)
	result.Where("y").Find(nil) // OK: returns immutable
}

// PIR403: combined directive after opening brace (FuncDecl)
func sameLinePureImmutable(db *gorm.DB) *gorm.DB { //gormreuse:pure,immutable-return
	return db.Session(&gorm.Session{})
}

func useSameLinePureImmutable(db *gorm.DB) {
	result := sameLinePureImmutable(db)
	result.Where("x").Find(nil)
	result.Where("y").Find(nil) // OK: returns immutable
	db.Find(nil)                // OK: db not polluted (pure)
}

// =============================================================================
// HELPER - Global DB for testing
// =============================================================================

var globalDB *gorm.DB
