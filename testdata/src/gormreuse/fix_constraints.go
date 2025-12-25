//go:build ignore
// +build ignore

package internal

import "gorm.io/gorm"

// =============================================================================
// Nested Call Patterns - Testing Reassignment vs Session Strategy Selection
//
// CRITICAL RULE: Reassignment (q = q.Method(...)) is ONLY valid when:
// 1. The *gorm.DB call is the DIRECT expression in an ExprStmt
// 2. The call is NOT nested inside another function call
//
// For nested calls (e.g., require.NoError(t, db.Save(...).Error)),
// we can ONLY use Session strategy, NOT reassignment.
// =============================================================================

// =============================================================================
// DIRECT CALLS - Can use reassignment strategy
// =============================================================================

// directNonFinisher demonstrates direct *gorm.DB call as ExprStmt.
// Reassignment IS valid here.
func directNonFinisher(db *gorm.DB) {
	q := db.Where("base")
	q.Find(nil) // First use - pollutes q

	// Direct call - ExprStmt.X IS the *gorm.DB call
	// Fix: q = q.Where("a")
	q.Where("a") // want `\*gorm\.DB instance reused after chain method`
}

// directChainedNonFinisher demonstrates direct chained *gorm.DB call.
// Reassignment IS valid - reassign to the entire chain result.
func directChainedNonFinisher(db *gorm.DB) {
	q := db.Where("base")
	q.Find(nil) // First use - pollutes q

	// Direct chained call - ExprStmt.X IS the outermost *gorm.DB call
	// Fix: q = q.Where("a").Where("b").Order("c")
	q.Where("a").Where("b").Order("c") // want `\*gorm\.DB instance reused after chain method`
}

// multipleDirectNonFinishers demonstrates multiple direct non-finisher calls.
// Each gets reassignment, creating new roots progressively.
func multipleDirectNonFinishers(db *gorm.DB) {
	q := db.Where("base")

	// Fix: q = q.Where("a")
	q.Where("a")  // want `\*gorm\.DB instance reused after chain method`
	// Fix: q = q.Where("b")
	q.Where("b")  // want `\*gorm\.DB instance reused after chain method`
	// Fix: q = q.Order("c")
	q.Order("c")  // want `\*gorm\.DB instance reused after chain method`

	q.Find(nil)   // First actual finisher - OK after reassignments
}

// directWithFinisher demonstrates mix of non-finisher and finisher.
func directWithFinisher(db *gorm.DB) {
	q := db.Where("base")

	// Fix: q = q.Where("a")
	q.Where("a")   // want `\*gorm\.DB instance reused after chain method`
	q.Find(nil)    // Finisher - OK (first use)
	// Fix: q = q.Where("b")
	q.Where("b")   // want `\*gorm\.DB instance reused after chain method`
	q.Count(nil)   // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// NESTED IN require.NoError - CANNOT use reassignment
// =============================================================================

// nestedInRequireNoError demonstrates *gorm.DB nested in require.NoError.
// Reassignment is NOT valid - require.NoError returns void!
// Must use Session strategy only.
func nestedInRequireNoError(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// Nested call - ExprStmt.X is require.NoError, NOT db.Create
	// require.NoError(t, db.Create(&struct{}{}).Error)
	// CANNOT add: db = require.NoError(...) <- require.NoError returns void!
	// MUST use: db = db.Session(&gorm.Session{}) before this line
	// Fix: db = db.Session(&gorm.Session{})
	db.Create(&struct{}{}) // want `\*gorm\.DB instance reused after chain method`
}

// nestedInRequireNoErrorChained demonstrates chained *gorm.DB in require.NoError.
func nestedInRequireNoErrorChained(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// Nested chained call
	// require.NoError(t, db.Model(&User{}).Where("id = ?", 1).Update("name", "x").Error)
	// CANNOT add: db = require.NoError(...)
	db.Model(&struct{}{}).Where("id = ?", 1).Update("name", "x") // want `\*gorm\.DB instance reused after chain method`
}

// multipleNestedInRequireNoError demonstrates multiple violations in require.NoError.
func multipleNestedInRequireNoError(db *gorm.DB) {
	db.Create(&struct{ ID int }{ID: 1}) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// Multiple nested calls - all violations
	db.Create(&struct{ ID int }{ID: 2}) // want `\*gorm\.DB instance reused after chain method`
	db.Create(&struct{ ID int }{ID: 3}) // want `\*gorm\.DB instance reused after chain method`
	db.Save(&struct{ ID int }{ID: 4})   // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// NESTED AS FUNCTION ARGUMENT - CANNOT use reassignment
// =============================================================================

// Helper function for testing argument passing
func helperThatTakesDB(db *gorm.DB) {}

// nestedAsFunctionArgument demonstrates *gorm.DB passed as function argument.
// Reassignment is NOT valid - we can't reassign function call result.
func nestedAsFunctionArgument(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// Nested as argument - ExprStmt.X is helperThatTakesDB, NOT db.Where
	// helperThatTakesDB(db.Where("x"))
	// CANNOT add: db = helperThatTakesDB(...) <- function doesn't return *gorm.DB
	helperThatTakesDB(db.Where("x")) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// NESTED IN RECEIVER CALL - CANNOT use reassignment
// =============================================================================

// Helper type for testing method receiver
type TestHelper struct{}

func (h *TestHelper) TruncateTables(db *gorm.DB) {}

// nestedInReceiverCall demonstrates *gorm.DB passed to method on receiver.
// This is the memtests.TruncateSchemaTables(db) pattern from the bug report.
func nestedInReceiverCall(db *gorm.DB, helper *TestHelper) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// Nested in receiver method call - ExprStmt.X is helper.TruncateTables, NOT db
	// helper.TruncateTables(db)
	// CANNOT add: helper = helper.TruncateTables(db) <- wrong variable!
	// CANNOT add: db = helper.TruncateTables(db) <- method doesn't return *gorm.DB
	helper.TruncateTables(db) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// MIXED: Direct and Nested in Same Function
// =============================================================================

// mixedDirectAndNested demonstrates both patterns in one function.
func mixedDirectAndNested(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// Direct call - CAN use reassignment
	db.Where("a") // want `\*gorm\.DB instance reused after chain method`

	// Nested call - CANNOT use reassignment
	// require.NoError(t, db.Create(&struct{}{}).Error)
	db.Create(&struct{}{}) // want `\*gorm\.DB instance reused after chain method`

	// Direct call again - CAN use reassignment
	db.Order("x") // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// FOLLOWED BY .Error - CANNOT use reassignment (returns error, not *gorm.DB)
// =============================================================================

// directCallWithError demonstrates direct *gorm.DB call followed by .Error.
// Even though it's direct, we can't use reassignment because .Error returns error.
func directCallWithError(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// db.Save(&struct{}{}).Error <- Returns error, not *gorm.DB
	// CANNOT add: db = db.Save(&struct{}{}).Error <- Type mismatch!
	// MUST use: db = db.Session(...) before this line
	_ = db.Save(&struct{}{}).Error // want `\*gorm\.DB instance reused after chain method`
}

// chainedCallWithError demonstrates chained call followed by .Error.
func chainedCallWithError(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// db.Where("x").Save(&struct{}{}).Error
	// CANNOT use reassignment - returns error
	_ = db.Where("x").Save(&struct{}{}).Error // want `\*gorm\.DB instance reused after chain method`
}

// errorInAssignment demonstrates .Error in assignment.
func errorInAssignment(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// Assignment to error variable
	err := db.Save(&struct{}{}).Error // want `\*gorm\.DB instance reused after chain method`
	_ = err
}

// =============================================================================
// EDGE CASES
// =============================================================================

// doubleNested demonstrates *gorm.DB nested twice.
// This tests that we find the innermost *gorm.DB call correctly.
func doubleNested(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// Double nested: outerFunc(require.NoError(t, db.Save(...).Error))
	// The innermost *gorm.DB call is db.Save
	// But it's nested inside require.NoError, which is nested inside outerFunc
	// CANNOT use reassignment
	db.Save(&struct{}{}) // want `\*gorm\.DB instance reused after chain method`
}

// nestedInSelectorChain demonstrates *gorm.DB in a selector chain.
func nestedInSelectorChain(db *gorm.DB) {
	db.Find(nil) // First use - pollutes db
	// want `\*gorm\.DB instance reused after chain method`

	// db.Where("x") is nested inside .Error selector
	// require.NoError(t, db.Where("x").Find(nil).Error)
	db.Where("x").Find(nil) // want `\*gorm\.DB instance reused after chain method`
}
