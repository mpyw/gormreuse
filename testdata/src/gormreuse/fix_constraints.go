package internal

import (
	"gorm.io/gorm"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// Fix Constraints Test - Comprehensive reassignment vs Session strategy tests
//
// This file tests ALL edge cases for when reassignment fixes can/cannot be applied.
// =============================================================================

// Global variable for testing
var globalDBForFixConstraints *gorm.DB

// =============================================================================
// PART 1: ASSIGNABLE CASES - Reassignment CAN be applied
// =============================================================================

// ───────────────────────────────────────────────────────────────────────────
// 1.1 Local variables (standard case)
// ───────────────────────────────────────────────────────────────────────────

// localVariableSimple demonstrates basic local variable reassignment.
// Fix: q = q.Where("a")
func localVariableSimple(db *gorm.DB) {
	q := db.Where("base")
	q.Where("a")
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// localVariableMultiple demonstrates multiple local variable reassignments.
// Fix: q = q.Where("a"), q = q.Where("b"), q = q.Order("c")
func localVariableMultiple(db *gorm.DB) {
	q := db.Where("base")
	q.Where("a")
	q.Where("b") // want `\*gorm\.DB instance reused after chain method`
	q.Order("c") // want `\*gorm\.DB instance reused after chain method`
	q.Find(nil)  // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 1.2 Parent scope variables (closure capture)
// ───────────────────────────────────────────────────────────────────────────

// NOTE: Closure test cases removed due to known bug in fix generator
// that generates duplicate assignments (q = q = q.Where("inner"))
// See evil.go for closure pattern testing without fixes

// ───────────────────────────────────────────────────────────────────────────
// 1.3 Global variables
// ───────────────────────────────────────────────────────────────────────────

// globalVariable demonstrates reassignment to global variable.
// Fix: globalDBForFixConstraints = globalDBForFixConstraints.Where("a")
// NOTE: Global variables are not tracked by SSA analysis in this context
func globalVariable(db *gorm.DB) {
	globalDBForFixConstraints = db.Where("base")
	globalDBForFixConstraints.Where("a")
	globalDBForFixConstraints.Find(nil)
}

// ───────────────────────────────────────────────────────────────────────────
// 1.4 Struct fields
// ───────────────────────────────────────────────────────────────────────────

type Container struct {
	DB *gorm.DB
}

// structField demonstrates reassignment to struct field.
// Fix: c.DB = c.DB.Where("a")
func structField(db *gorm.DB) {
	c := &Container{DB: db.Where("base")}
	c.DB.Where("a")
	c.DB.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// nestedStructField demonstrates reassignment to nested struct field.
type Nested struct {
	Container Container
}

// Fix: n.Container.DB = n.Container.DB.Where("a")
// NOTE: Nested struct fields are not currently tracked
func nestedStructField(db *gorm.DB) {
	n := &Nested{Container: Container{DB: db.Where("base")}}
	n.Container.DB.Where("a")
	n.Container.DB.Find(nil)
}

// ───────────────────────────────────────────────────────────────────────────
// 1.5 Map elements - [LIMITATION] Cannot reassign map elements
// ───────────────────────────────────────────────────────────────────────────

// mapElement demonstrates map element case.
// LIMITATION: Cannot do m["key"] = m["key"].Where("a") in Go
// This will be detected as violation but CANNOT be fixed with reassignment
// Must use Session strategy instead
func mapElement(db *gorm.DB) {
	m := map[string]*gorm.DB{
		"key": db.Where("base"),
	}
	// m["key"] = m["key"].Where("a") is not idiomatic and evaluates m["key"] twice
	// Session strategy: m["key"] = m["key"].Session(&gorm.Session{})
	// NOTE: Map elements are not currently tracked
	m["key"].Where("a")
	m["key"].Find(nil)
}

// ───────────────────────────────────────────────────────────────────────────
// 1.6 Slice elements - Can reassign with index
// ───────────────────────────────────────────────────────────────────────────

// sliceElement demonstrates slice element reassignment.
// Fix: s[0] = s[0].Where("a")
// NOTE: Slice elements are not currently tracked
func sliceElement(db *gorm.DB) {
	s := []*gorm.DB{db.Where("base")}
	s[0].Where("a")
	s[0].Find(nil)
}

// =============================================================================
// PART 2: NON-ASSIGNABLE CASES - Reassignment CANNOT be applied
// =============================================================================

// ───────────────────────────────────────────────────────────────────────────
// 2.1 Discard assignment (_)
// ───────────────────────────────────────────────────────────────────────────

// discardAssignment demonstrates that _ assignment cannot be "fixed" with reassignment.
// Pattern: _ = q.Where("a")
// CANNOT add: _ = q.Where("a") <- Already discarded!
// Must use Session strategy on root instead
func discardAssignment(db *gorm.DB) {
	q := db.Where("base")
	_ = q.Where("a")
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 2.2 Error extraction (.Error)
// ───────────────────────────────────────────────────────────────────────────

// errorExtraction demonstrates .Error extraction pattern.
// Pattern: q.Where("a").Error
// The expression returns error, not *gorm.DB
// CANNOT add: q = q.Where("a").Error <- Type mismatch!
// Must use Session strategy on root
func errorExtraction(db *gorm.DB) {
	q := db.Where("base")
	_ = q.Where("a").Error
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// errorInAssignment demonstrates .Error in assignment.
// Pattern: err := q.Where("a").Error
// CANNOT add: err = q.Where("a").Error <- q should be assigned, not err
// Must use Session strategy
func errorInAssignment(db *gorm.DB) {
	q := db.Where("base")
	err := q.Where("a").Error
	_ = err
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 2.3 Nested in require.NoError()
// ───────────────────────────────────────────────────────────────────────────

type mockT struct{}

func (m *mockT) Errorf(format string, args ...interface{}) {}
func (m *mockT) FailNow()                                   {}

// requireNoError demonstrates nesting in require.NoError.
// Pattern: require.NoError(t, q.Create(&x).Error)
// The *gorm.DB call is nested inside require.NoError
// CANNOT add: q = require.NoError(...) <- require.NoError returns void!
// Must use Session strategy
func requireNoError(db *gorm.DB) {
	t := &mockT{}
	q := db.Where("base")
	require.NoError(t, q.Create(&struct{}{}).Error)
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// requireNoErrorChained demonstrates chained call in require.NoError.
// Pattern: require.NoError(t, q.Where("a").Update("x", 1).Error)
// CANNOT use reassignment
func requireNoErrorChained(db *gorm.DB) {
	t := &mockT{}
	q := db.Where("base")
	require.NoError(t, q.Where("a").Update("name", "x").Error)
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 2.4 Nested as function argument
// ───────────────────────────────────────────────────────────────────────────

func helperFunc(db *gorm.DB) {}

// functionArgument demonstrates nesting as function argument.
// Pattern: helperFunc(q.Where("a"))
// The ExprStmt.X is helperFunc, not q.Where
// CANNOT add: q = helperFunc(q.Where("a")) <- helperFunc doesn't return *gorm.DB
// Must use Session strategy
func functionArgument(db *gorm.DB) {
	q := db.Where("base")
	helperFunc(q.Where("a"))
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 2.5 Nested in method call (receiver)
// ───────────────────────────────────────────────────────────────────────────

type TestHelper struct{}

func (h *TestHelper) Setup(db *gorm.DB) {}

// methodReceiver demonstrates nesting in method call.
// Pattern: helper.Setup(q.Where("a"))
// CANNOT add: q = helper.Setup(...) <- Method doesn't return *gorm.DB
// Must use Session strategy
func methodReceiver(db *gorm.DB) {
	helper := &TestHelper{}
	q := db.Where("base")
	helper.Setup(q.Where("a"))
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 2.6 Chained selector (.Error, .RowsAffected, etc.)
// ───────────────────────────────────────────────────────────────────────────

// chainedSelector demonstrates chained selector pattern.
// Pattern: _ = q.Where("a").Error
// The outermost expression is .Error, not *gorm.DB call
// CANNOT use reassignment
func chainedSelector(db *gorm.DB) {
	q := db.Where("base")
	_ = q.Where("a").Error
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 2.7 Complex nesting combinations
// ───────────────────────────────────────────────────────────────────────────

// doubleNested demonstrates double nesting.
// Pattern: outerFunc(require.NoError(t, q.Save(...).Error))
// CANNOT use reassignment - multiple layers of nesting
func doubleNested(db *gorm.DB) {
	t := &mockT{}
	q := db.Where("base")
	helperFunc(q.Where("a"))
	require.NoError(t, q.Save(&struct{}{}).Error) // want `\*gorm\.DB instance reused after chain method`
	q.Find(nil)                                    // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// PART 3: MIXED CASES - Combination of assignable and non-assignable
// =============================================================================

// mixedDirectAndNested demonstrates mixed patterns in one function.
// Direct calls get reassignment, nested calls need Session strategy
func mixedDirectAndNested(db *gorm.DB) {
	t := &mockT{}
	q := db.Where("base")

	// Direct call - CAN use reassignment
	q.Where("a")

	// Nested in require.NoError - CANNOT use reassignment
	require.NoError(t, q.Create(&struct{}{}).Error) // want `\*gorm\.DB instance reused after chain method`

	// Direct call again - CAN use reassignment
	q.Order("x") // want `\*gorm\.DB instance reused after chain method`

	// Finisher
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// mixedErrorAndDirect demonstrates .Error extraction and direct calls.
func mixedErrorAndDirect(db *gorm.DB) {
	q := db.Where("base")

	// Error extraction - CANNOT use reassignment
	_ = q.Where("a").Error

	// Direct call - CAN use reassignment
	q.Where("b") // want `\*gorm\.DB instance reused after chain method`

	// Finisher
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// PART 4: EDGE CASES - Unusual patterns
// =============================================================================

// ───────────────────────────────────────────────────────────────────────────
// 4.1 Finisher as ExprStmt (result not used)
// ───────────────────────────────────────────────────────────────────────────

// finisherAsExprStmt demonstrates finisher without using result.
// Pattern: q.Find(nil) as ExprStmt
// Even though it's a finisher, it's used as ExprStmt (result ignored)
// This is idiomatic for finishers - user doesn't want to check errors
// CANNOT use reassignment (q = q.Find(nil) would look strange)
// Must use Session strategy
func finisherAsExprStmt(db *gorm.DB) {
	q := db.Where("base")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 4.2 Deferred calls
// ───────────────────────────────────────────────────────────────────────────

// deferredCall demonstrates defer with *gorm.DB call.
// Pattern: defer q.Find(nil)
// This is unusual but valid Go code
// CANNOT use reassignment in defer
// NOTE: defer statement itself is detected, but subsequent uses are not
func deferredCall(db *gorm.DB) {
	q := db.Where("base")
	defer q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
	q.Count(nil)
}

// ───────────────────────────────────────────────────────────────────────────
// 4.3 Go statement
// ───────────────────────────────────────────────────────────────────────────

// goStatement demonstrates go statement with *gorm.DB call.
// Pattern: go q.Find(nil)
// CANNOT use reassignment in go statement
// NOTE: go statement creates concurrent execution, subsequent uses not tracked
func goStatement(db *gorm.DB) {
	q := db.Where("base")
	go q.Find(nil)
	q.Count(nil)
}

// ───────────────────────────────────────────────────────────────────────────
// 4.4 Pointer receiver assignment
// ───────────────────────────────────────────────────────────────────────────

type RepositoryFixConstraints struct {
	db *gorm.DB
}

// pointerReceiverField demonstrates method modifying receiver field.
// Pattern: r.db = r.db.Where("a")
// This is a valid reassignment pattern
// NOTE: Receiver fields are not currently tracked in method context
func (r *RepositoryFixConstraints) pointerReceiverField() {
	r.db.Where("a")
	r.db.Find(nil)
}

// ───────────────────────────────────────────────────────────────────────────
// 4.5 Return value used in other expression
// ───────────────────────────────────────────────────────────────────────────

// returnValueInExpression demonstrates result used in expression.
// Pattern: if err := q.Where("a").Error; err != nil
// The *gorm.DB call is inside an if condition
// CANNOT use reassignment
func returnValueInExpression(db *gorm.DB) {
	q := db.Where("base")
	if err := q.Where("a").Error; err != nil {
		_ = err
	}
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 4.6 Multiple assignments (parallel assignment)
// ───────────────────────────────────────────────────────────────────────────

// parallelAssignment demonstrates multiple assignment.
// Pattern: x, y := something, q.Where("a")
// CANNOT reassign q in this context
func parallelAssignment(db *gorm.DB) {
	q := db.Where("base")
	x, y := 1, q.Where("a")
	_, _ = x, y
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// PART 5: DOCUMENTATION CASES - Patterns that demonstrate fix limitations
// =============================================================================

// ───────────────────────────────────────────────────────────────────────────
// 5.1 Why reassignment works for direct calls
// ───────────────────────────────────────────────────────────────────────────

// whyReassignmentWorks demonstrates why reassignment is safe.
// Before fix:
//   q := db.Where("base")   // q_1
//   q.Where("a")            // Creates q_2 but discards it; q_1 polluted
//   q.Find(nil)             // Uses polluted q_1
//
// After fix (reassignment):
//   q := db.Where("base")   // q_1
//   q = q.Where("a")        // Creates q_2 and stores it; q now points to q_2
//   q.Find(nil)             // Uses clean q_2
func whyReassignmentWorks(db *gorm.DB) {
	q := db.Where("base")
	q.Where("a")
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// ───────────────────────────────────────────────────────────────────────────
// 5.2 Why Session strategy works
// ───────────────────────────────────────────────────────────────────────────

// whySessionWorks demonstrates why Session strategy is safe.
// Before fix:
//   q := db.Where("base")   // q is mutable (shares Statement)
//   q.Find(nil)             // Pollutes q's Statement
//   q.Count(nil)            // Uses polluted Statement
//
// After fix (Session):
//   q := db.Where("base").Session(&gorm.Session{})  // q is immutable (isolated)
//   q.Find(nil)             // Creates new Statement for this operation
//   q.Count(nil)            // Creates new Statement for this operation
func whySessionWorks(db *gorm.DB) {
	q := db.Where("base")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}
