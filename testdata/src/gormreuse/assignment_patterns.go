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

// intermediateVariable demonstrates intermediate variable in chain.
func intermediateVariable(db *gorm.DB) {
	q := db.Where("x")
	tmp := q.Where("y") // Assignment to tmp, but not to q
	tmp.Find(nil)
}
