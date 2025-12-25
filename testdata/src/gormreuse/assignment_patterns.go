//go:build ignore
// +build ignore

package internal

import "gorm.io/gorm"

// =============================================================================
// Assignment Detection Test Cases
// These test cases verify that the isAssignment() heuristic correctly
// distinguishes between assignments (which create new roots) and
// immediate consumption (which pollutes the root).
// =============================================================================

// =============================================================================
// SHOULD BE DETECTED AS ASSIGNMENT (Phi or Store to Alloc)
// =============================================================================

// explicitStoreToAlloc demonstrates Store-to-Alloc pattern explicitly.
// SSA: t1 = Alloc *gorm.DB
//      t2 = db.Where("x")
//      Store t1 <- t2     ← Store to Alloc (assignment)
//      t3 = UnOp * t1
//      t4 = t3.Where("y")
//      Store t1 <- t4     ← Store to Alloc (assignment)
func explicitStoreToAlloc(db *gorm.DB) {
	var q *gorm.DB     // Alloc
	q = db.Where("x")  // Store to Alloc - ASSIGNMENT
	q = q.Where("y")   // Store to Alloc - ASSIGNMENT
	q.Find(nil)        // First actual use - OK
}

// simpleAssignment demonstrates basic reassignment.
// SSA: q_2 = call q_1.Where("y")
//      Store alloc_q, q_2  OR  q_3 = Phi(q_1, q_2)
func simpleAssignment(db *gorm.DB) {
	q := db.Where("x")
	q = q.Where("y") // ASSIGNMENT: result goes to Phi or Store
	_ = q
}

// conditionalAssignmentOneBranch demonstrates assignment in one branch.
// SSA: Block merge has Phi(q_1, q_2) where q_2 = call q_1.Where("y")
func conditionalAssignmentOneBranch(db *gorm.DB, flag bool) {
	q := db.Where("x")
	if flag {
		q = q.Where("y") // ASSIGNMENT: q_2 goes into Phi
	}
	_ = q // q_3 = Phi(q_1, q_2)
}

// conditionalAssignmentBothBranches demonstrates assignment in both branches.
// SSA: Phi(q_1, q_2) where both q_1 and q_2 are call results
func conditionalAssignmentBothBranches(db *gorm.DB, flag bool) {
	var q *gorm.DB
	if flag {
		q = db.Where("x") // ASSIGNMENT: q_1 goes into Phi
	} else {
		q = db.Where("y") // ASSIGNMENT: q_2 goes into Phi
	}
	_ = q // q_3 = Phi(q_1, q_2)
}

// loopAssignment demonstrates assignment in loop.
// SSA: Loop header has Phi(q_1, q_next) where q_next = call ...
func loopAssignment(db *gorm.DB) {
	q := db.Where("x")
	for i := 0; i < 10; i++ {
		q = q.Where("y") // ASSIGNMENT: q_next goes into loop Phi
	}
	_ = q
}

// switchAssignment demonstrates assignment in switch.
// SSA: After switch has Phi merging all case results
func switchAssignment(db *gorm.DB, n int) {
	q := db.Where("x")
	switch n {
	case 1:
		q = q.Where("a") // ASSIGNMENT: goes into Phi
	case 2:
		q = q.Where("b") // ASSIGNMENT: goes into Phi
	}
	_ = q // Phi(q_1, q_a, q_b)
}

// nestedIfAssignment demonstrates nested conditional assignment.
func nestedIfAssignment(db *gorm.DB, flag1, flag2 bool) {
	q := db.Where("x")
	if flag1 {
		if flag2 {
			q = q.Where("y") // ASSIGNMENT: goes into nested Phi
		}
	}
	_ = q
}

// loopWithBreakAssignment demonstrates assignment in loop with break.
func loopWithBreakAssignment(db *gorm.DB) {
	q := db.Where("x")
	for i := 0; i < 10; i++ {
		if i > 5 {
			break
		}
		q = q.Where("y") // ASSIGNMENT: goes into loop Phi
	}
	_ = q
}

// earlyReturnWithAssignment demonstrates assignment before return.
func earlyReturnWithAssignment(db *gorm.DB, flag bool) {
	q := db.Where("x")
	if flag {
		q = q.Where("y") // ASSIGNMENT: goes into Phi
		return
	}
	_ = q // Phi includes q from early return path
}

// =============================================================================
// SHOULD NOT BE DETECTED AS ASSIGNMENT (Immediate consumption)
// =============================================================================

// immediateConsumption demonstrates method chaining without assignment.
// SSA: tmp = call q.Where("y")
//      call tmp.Find(nil)
// tmp's referrer is the Find Call, not Phi
func immediateConsumption(db *gorm.DB) {
	q := db.Where("x")
	q.Where("y").Find(nil) // NOT ASSIGNMENT: result immediately consumed
}

// conditionalImmediateConsumption demonstrates immediate consumption in branch.
func conditionalImmediateConsumption(db *gorm.DB, flag bool) {
	q := db.Where("x")
	if flag {
		q.Where("y").Find(nil) // NOT ASSIGNMENT: immediately consumed
	}
}

// loopImmediateConsumption demonstrates immediate consumption in loop.
func loopImmediateConsumption(db *gorm.DB) {
	q := db.Where("x")
	for i := 0; i < 10; i++ {
		q.Where("y").Find(nil) // NOT ASSIGNMENT: immediately consumed
	}
}

// differentVariableAssignment demonstrates assignment to different variable.
// This creates a NEW Alloc, not reassignment to existing variable
func differentVariableAssignment(db *gorm.DB) {
	q := db.Where("x")
	q2 := q.Where("y") // NOT ASSIGNMENT OF q: q2 is new Alloc
	_, _ = q, q2
}

// returnValue demonstrates returning without assignment.
func returnValue(db *gorm.DB) *gorm.DB {
	q := db.Where("x")
	return q.Where("y") // NOT ASSIGNMENT: returned
}

// functionArgument demonstrates passing as argument without assignment.
func functionArgument(db *gorm.DB) {
	helper := func(x *gorm.DB) {}
	q := db.Where("x")
	helper(q.Where("y")) // NOT ASSIGNMENT: passed as argument
}

// earlyReturnWithoutAssignment demonstrates early return without assignment.
func earlyReturnWithoutAssignment(db *gorm.DB, flag bool) {
	q := db.Where("x")
	if flag {
		q.Where("y").Find(nil) // NOT ASSIGNMENT: consumed before return
		return
	}
	_ = q
}

// unusedCallResult demonstrates call with no referrers (result unused).
// This tests the `if call.Referrers() == nil` path in isAssignment.
func unusedCallResult(db *gorm.DB) {
	q := db.Where("x")
	q.Where("y") // Result unused - no referrers, not an assignment
	q.Find(nil)  // First actual use - OK
}

// storeToStructField demonstrates Store to FieldAddr (not Alloc).
// This tests the Store-but-not-Alloc path in isAssignment.
// Also tests StoreHandler's early return for non-IndexAddr stores.
func storeToStructField(db *gorm.DB) {
	type Container struct {
		DB *gorm.DB
	}
	c := &Container{}
	c.DB = db.Where("x") // Store to FieldAddr (not Alloc/IndexAddr)
	c.DB.Find(nil)       // Use - OK (not tracked as pollution)
}

// storeToSliceElement demonstrates Store to IndexAddr (not Alloc).
func storeToSliceElement(db *gorm.DB) {
	slice := make([]*gorm.DB, 1)
	q := db.Where("x")
	slice[0] = q.Where("y") // Store to IndexAddr (not Alloc)
	q.Find(nil)             // First use of q - OK
}

// chainedWithSend demonstrates channel send (tests SendHandler coverage).
func chainedWithSend(db *gorm.DB) {
	ch := make(chan *gorm.DB, 1)
	q := db.Where("x")
	ch <- q // Send to channel - pollutes q
	// want "should call Session before reusing"
	q.Find(nil) // Second use - VIOLATION
}

// chainedWithMapStore demonstrates map storage (tests MapUpdateHandler coverage).
func chainedWithMapStore(db *gorm.DB) {
	m := make(map[string]*gorm.DB)
	q := db.Where("x")
	m["key"] = q // Store in map - pollutes q
	// want "should call Session before reusing"
	q.Find(nil) // Second use - VIOLATION
}

// chainedWithInterfaceConversion demonstrates interface conversion (tests MakeInterfaceHandler).
func chainedWithInterfaceConversion(db *gorm.DB) {
	q := db.Where("x")
	var i interface{} = q // Convert to interface - pollutes q
	_ = i
	// want "should call Session before reusing"
	q.Find(nil) // Second use - VIOLATION
}

// immutableRootWithSend tests SendHandler with immutable root (root == nil path).
func immutableRootWithSend(db *gorm.DB) {
	ch := make(chan *gorm.DB, 1)
	q := db.Session(&gorm.Session{}).Where("x") // Immutable root
	ch <- q    // Send to channel - OK with immutable root
	q.Find(nil) // First use - OK
}

// immutableRootWithMapStore tests MapUpdateHandler with immutable root.
func immutableRootWithMapStore(db *gorm.DB) {
	m := make(map[string]*gorm.DB)
	q := db.Session(&gorm.Session{}).Where("x") // Immutable root
	m["key"] = q // Store in map - OK with immutable root
	q.Find(nil)  // First use - OK
}

// immutableRootWithSliceStore tests StoreHandler with immutable root.
func immutableRootWithSliceStore(db *gorm.DB) {
	slice := make([]*gorm.DB, 1)
	q := db.Session(&gorm.Session{}).Where("x") // Immutable root
	slice[0] = q // Store to slice - OK with immutable root
	q.Find(nil)  // First use - OK
}

// immutableRootWithInterfaceConversion tests MakeInterfaceHandler with immutable root.
func immutableRootWithInterfaceConversion(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x") // Immutable root
	var i interface{} = q // Convert to interface - OK with immutable root
	_ = i
	q.Find(nil) // First use - OK
}

//gormreuse:pure
func pureHelper(db *gorm.DB) *gorm.DB {
	// This function is marked as pure - doesn't pollute the argument
	return db
}

// testPureFunction tests IsPureFunction path in tracer.
func testPureFunction(db *gorm.DB) {
	q := db.Where("x")
	q2 := pureHelper(q) // Pure function - doesn't pollute q
	q.Find(nil)        // First use - OK
	q2.Count(nil)      // Use of q2 - OK
}

//gormreuse:immutable-return
func immutableReturningHelper(db *gorm.DB) *gorm.DB {
	// This function returns immutable *gorm.DB
	return db.Session(&gorm.Session{})
}

// testImmutableReturningFunction tests IsImmutableReturningBuiltin path.
func testImmutableReturningFunction(db *gorm.DB) {
	q := immutableReturningHelper(db) // Immutable-returning function
	q.Find(nil) // First use - OK
	q.Count(nil) // Second use - OK (q is immutable)
}

// testWithContext tests WithContext immutable-returning builtin.
func testWithContext(db *gorm.DB) {
	q := db.WithContext(nil) // Immutable-returning builtin
	q.Find(nil)  // First use - OK
	q.Count(nil) // Second use - OK (q is immutable)
}

// testDebug tests Debug immutable-returning builtin.
func testDebug(db *gorm.DB) {
	q := db.Debug() // Immutable-returning builtin
	q.Find(nil)  // First use - OK
	q.Count(nil) // Second use - OK (q is immutable)
}

// testBoundMethod tests bound method calls (method values).
// This tests processBoundMethodCall in CallHandler.
func testBoundMethod(db *gorm.DB) {
	q := db.Where("x")
	find := q.Find // Create method value (bound method)
	// want "should call Session before reusing"
	find(nil)      // First use via bound method - pollutes q
	// want "should call Session before reusing"
	q.Count(nil)   // Second use - VIOLATION
}

// testBoundMethodImmutable tests bound method with immutable root.
func testBoundMethodImmutable(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x") // Immutable root
	find := q.Find // Create method value
	find(nil)    // First use - OK
	q.Count(nil) // Second use - OK (immutable root)
}

// testGoStatement tests goroutine with db usage (GoHandler).
func testGoStatement(db *gorm.DB) {
	q := db.Where("x")
	// want "should call Session before reusing"
	go q.Find(nil) // Goroutine - pollutes q
	// want "should call Session before reusing"
	q.Count(nil)   // Second use - VIOLATION
}

// testDeferStatement tests defer with db usage (DeferHandler).
func testDeferStatement(db *gorm.DB) {
	q := db.Where("x")
	q.Find(nil)       // First use - pollutes q
	// want "should call Session before reusing"
	defer q.Count(nil) // Defer with polluted q - VIOLATION
}

// testDeferBeforeUse tests defer before actual use.
func testDeferBeforeUse(db *gorm.DB) {
	q := db.Where("x")
	// want "should call Session before reusing"
	defer q.Find(nil) // Defer - will execute at function exit
	// want "should call Session before reusing"
	q.Count(nil)      // Use before defer - VIOLATION (both pollute q)
}

// intermediateVariable demonstrates intermediate variable in chain.
func intermediateVariable(db *gorm.DB) {
	q := db.Where("x")
	tmp := q.Where("y") // Assignment to tmp, but not to q
	tmp.Find(nil)
}
