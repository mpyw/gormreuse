// Package internal contains end-to-end tests that verify actual GORM SQL
// behavior with sqlmock. They guard the linter's core premise: a mid-chain
// (clone==0) *gorm.DB shares its Statement, so branching it twice accumulates
// conditions, whereas root / Session()-derived (clone>0) handles fork a fresh
// Statement per chain and are safe to reuse. A Session()/Debug() copy of a
// mid-chain value still reads that value's Statement, though, so later changes
// to the value leak into it (#174). Every test asserts (t.Errorf) — none merely
// logs both outcomes.
package internal

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// User is a test model.
type User struct {
	ID     uint
	Name   string
	Active bool
	Age    int
}

// setupDB creates a GORM DB with sqlmock and a SQL-capture callback.
func setupDB(t *testing.T) (*gorm.DB, *[]string) {
	t.Helper()
	db, captured, _ := setupDBWithMock(t, false)
	return db, captured
}

// setupDBWithMock creates a GORM DB with sqlmock. When withTx is true it also
// expects a Begin/Commit pair (ordered before/after the queries) so tests that
// use Transaction() work. Returns the DB, a pointer to the captured SQL slice,
// and the mock for adding further expectations.
func setupDBWithMock(t *testing.T, withTx bool) (*gorm.DB, *[]string, sqlmock.Sqlmock) {
	t.Helper()

	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("Failed to create sqlmock: %v", err)
	}
	// Assertions read the captured SQL, not mock ordering; unordered matching
	// keeps the surplus ExpectQuery stubs from clashing with Begin/Commit.
	mock.MatchExpectationsInOrder(false)

	if withTx {
		mock.ExpectBegin()
	}
	for range 20 {
		mock.ExpectQuery(".*").WillReturnRows(
			sqlmock.NewRows([]string{"count", "id", "name", "active", "age"}).AddRow(100, 1, "test", true, 20),
		)
	}
	if withTx {
		mock.ExpectCommit()
	}

	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      mockDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		SkipDefaultTransaction: true,
		Logger:                 logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("Failed to open gorm: %v", err)
	}

	var capturedSQL []string
	if err := db.Callback().Query().After("gorm:query").Register("capture", func(tx *gorm.DB) {
		if tx.Statement != nil {
			capturedSQL = append(capturedSQL, tx.Statement.SQL.String())
		}
	}); err != nil {
		t.Fatalf("Failed to register capture callback: %v", err)
	}

	return db, &capturedSQL, mock
}

// TestSessionAtEnd verifies that Session() at the end of a chain isolates it:
// each finisher runs an independent query with only the base condition.
func TestSessionAtEnd(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1).Session(&gorm.Session{})
	q.Count(new(int64))
	q.Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	for i, sql := range *captured {
		if count := strings.Count(sql, "base"); count != 1 {
			t.Errorf("query %d: 'base' appears %d times, want 1 (Session must isolate): %s", i, count, sql)
		}
	}
}

// TestSessionBeforeFinisher verifies that Session() before each finisher isolates.
func TestSessionBeforeFinisher(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1)
	q.Session(&gorm.Session{}).Count(new(int64))
	q.Session(&gorm.Session{}).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	for i, sql := range *captured {
		if count := strings.Count(sql, "base"); count != 1 {
			t.Errorf("query %d: 'base' appears %d times, want 1 (Session before finisher must isolate): %s", i, count, sql)
		}
	}
}

// TestMutableReuse asserts the linter's central premise: branching a mid-chain
// (clone==0) value twice ACCUMULATES conditions — the second query inherits the
// first branch's WHERE.
func TestMutableReuse(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1) // clone==0 (mid-chain)
	q.Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	first, second := (*captured)[0], (*captured)[1]
	if !strings.Contains(first, "base") || !strings.Contains(first, "a") {
		t.Errorf("first query should contain base and a: %s", first)
	}
	// The invariant: the second branch inherits "a" from the first (pollution).
	if !strings.Contains(second, "a") {
		t.Errorf("second query should have ACCUMULATED 'a' from the first branch (mutable reuse), got: %s", second)
	}
}

// TestSessionInMiddle asserts that Session() in the MIDDLE does not isolate:
// Where() after Session() yields a fresh clone==0 value, so branching it twice
// still accumulates (contrast with TestSessionAtEnd).
func TestSessionInMiddle(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Session(&gorm.Session{}).Where("base = ?", 1) // clone==0 again
	q.Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; !strings.Contains(second, "a") {
		t.Errorf("Session-in-middle leaves the value mutable, so the second branch should accumulate 'a', got: %s", second)
	}
}

// TestCountThenFind asserts that Count followed by Find on a mutable value runs
// two queries and — since Count adds no WHERE — does NOT accumulate the base
// condition (no captured query repeats "base"). Documents GORM's actual
// behavior for this common pattern; the exact captured SQL of each callback can
// vary by GORM version, so we only assert the stable non-accumulation invariant.
func TestCountThenFind(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1)
	q.Count(new(int64))
	q.Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	for i, sql := range *captured {
		if count := strings.Count(sql, "base"); count > 1 {
			t.Errorf("query %d accumulated 'base' %d times (Count+Find must not accumulate): %s", i, count, sql)
		}
	}
}

// =============================================================================
// Clone-semantics tests — executable evidence for Phase 1b (#61) and the README.
// =============================================================================

// TestCloneSemantics_RootReuseIsSafe asserts (a): the root DB (clone>0) forks a
// fresh Statement per chain, so reusing it across two chains yields INDEPENDENT
// queries — the second does not inherit the first's condition.
func TestCloneSemantics_RootReuseIsSafe(t *testing.T) {
	db, captured := setupDB(t)

	db.Model(&User{}).Where("a = ?", 10).Find(&[]User{})
	db.Model(&User{}).Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; strings.Contains(second, "a =") {
		t.Errorf("root reuse must be independent: second query leaked 'a' from the first: %s", second)
	}
}

// TestCloneSemantics_MidChainBranchAccumulates asserts (b): a mid-chain
// (clone==0) value branched twice shares its Statement, so conditions accumulate.
func TestCloneSemantics_MidChainBranchAccumulates(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1) // clone==0
	q.Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	second := (*captured)[1]
	if !strings.Contains(second, "a =") || !strings.Contains(second, "b =") {
		t.Errorf("mid-chain branch should accumulate both 'a' and 'b' in the second query: %s", second)
	}
}

// TestCloneSemantics_TransactionCallbackIsolated asserts (c): the tx handed to a
// Transaction callback is a fresh (clone>0) handle, so branching it twice inside
// the callback yields independent queries — no accumulation.
func TestCloneSemantics_TransactionCallbackIsolated(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, true)

	err := db.Transaction(func(tx *gorm.DB) error {
		tx.Model(&User{}).Where("a = ?", 10).Find(&[]User{})
		tx.Model(&User{}).Where("b = ?", 20).Find(&[]User{})
		return nil
	})
	if err != nil {
		t.Fatalf("Transaction returned error: %v", err)
	}

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; strings.Contains(second, "a =") {
		t.Errorf("Transaction tx reuse must be independent: second query leaked 'a': %s", second)
	}
}

// TestCloneSemantics_FindInBatchesCallbackIsolated asserts that the tx handed
// to a FindInBatches callback is fresh: gorm builds it with
// Session(&Session{NewDB: true}), so branching it twice inside the callback
// yields independent queries. This is the runtime premise of the analyzer's
// builtin FindInBatches exemption (#62, #173).
func TestCloneSemantics_FindInBatchesCallbackIsolated(t *testing.T) {
	db, captured := setupDB(t)

	var batch []User
	calls := 0
	err := db.Model(&User{}).FindInBatches(&batch, 100, func(tx *gorm.DB, _ int) error {
		calls++
		tx.Where("a = ?", 10).Find(&[]User{})
		tx.Where("b = ?", 20).Find(&[]User{})
		return nil
	}).Error
	if err != nil {
		t.Fatalf("FindInBatches returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Expected the callback to run once, got %d", calls)
	}

	// captured[0] is the batch query itself; the callback's two queries follow.
	if len(*captured) < 3 {
		t.Fatalf("Expected at least 3 queries, got %d: %v", len(*captured), *captured)
	}
	first, second := (*captured)[1], (*captured)[2]
	if !strings.Contains(first, "a =") {
		t.Errorf("first callback query should contain 'a': %s", first)
	}
	if strings.Contains(second, "a =") {
		t.Errorf("FindInBatches tx reuse must be independent: second query leaked 'a': %s", second)
	}
}

// TestCloneSemantics_ConnectionCallbackAccumulates asserts that, unlike
// Transaction and FindInBatches, the tx handed to a Connection callback is NOT
// fresh: gorm passes db.getInstance(), a clone==0 handle, so branching it twice
// inside the callback accumulates conditions. This is why the analyzer does not
// exempt Connection callbacks (#173).
func TestCloneSemantics_ConnectionCallbackAccumulates(t *testing.T) {
	db, captured := setupDB(t)

	err := db.Connection(func(tx *gorm.DB) error {
		tx.Model(&User{}).Where("a = ?", 10).Find(&[]User{})
		tx.Model(&User{}).Where("b = ?", 20).Find(&[]User{})
		return nil
	})
	if err != nil {
		t.Fatalf("Connection returned error: %v", err)
	}

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	second := (*captured)[1]
	if !strings.Contains(second, "a =") || !strings.Contains(second, "b =") {
		t.Errorf("Connection tx is clone==0, so the second query should accumulate both 'a' and 'b': %s", second)
	}
}

// TestDerivedIntoOtherVariableAccumulates asserts the runtime premise of #175: a
// chain derived from a mid-chain value into a different variable under a
// condition is a branch. It shares the Statement, so a later use of the
// original value inherits its condition.
func TestDerivedIntoOtherVariableAccumulates(t *testing.T) {
	db, captured := setupDB(t)

	derivedIntoOtherVariable(db.Model(&User{}).Where("base = ?", 1), true) // clone==0

	if len(*captured) < 1 {
		t.Fatalf("Expected at least 1 query, got %d", len(*captured))
	}
	if first := (*captured)[0]; !strings.Contains(first, "a =") {
		t.Errorf("q.Where into q2 shares q's Statement, so q's query should contain 'a': %s", first)
	}
}

func derivedIntoOtherVariable(q *gorm.DB, cond bool) {
	var q2 *gorm.DB
	if cond {
		q2 = q.Where("a = ?", 10)
	}
	q.Find(&[]User{})
	if q2 != nil {
		q2.Find(&[]User{})
	}
}

// TestConditionalReassignmentFromMidChainAccumulates asserts the runtime
// premise of #175 for a conditional reassignment that starts from the same
// mid-chain value twice: on the reassigning path, both chains share its
// Statement, so the final query carries both conditions.
func TestConditionalReassignmentFromMidChainAccumulates(t *testing.T) {
	db, captured := setupDB(t)

	conditionalReassign(db.Model(&User{}), true) // clone==0

	if len(*captured) < 1 {
		t.Fatalf("Expected at least 1 query, got %d", len(*captured))
	}
	if first := (*captured)[0]; !strings.Contains(first, "base =") || !strings.Contains(first, "alt =") {
		t.Errorf("both chains branch the clone==0 argument, so the query should contain 'base' and 'alt': %s", first)
	}
}

func conditionalReassign(db *gorm.DB, cond bool) {
	q := db.Where("base = ?", 1)
	if cond {
		q = db.Where("alt = ?", 2)
	}
	q.Find(&[]User{})
}

// =============================================================================
// Session/Debug copies of a mid-chain value (#174)
// =============================================================================

// TestCopyOfMidChain_LaterChangeLeaks asserts that Session(&gorm.Session{}) and
// Debug() on a clone==0 value return a copy that points at the same Statement:
// a change to the original after the copy is taken shows up in the copy. A
// change by reassignment (q = q.Where(...)) changes it in place too.
func TestCopyOfMidChain_LaterChangeLeaks(t *testing.T) {
	copies := map[string]func(q *gorm.DB) *gorm.DB{
		"Session": func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{}) },
		"Debug":   func(q *gorm.DB) *gorm.DB { return q.Debug() },
		"Session(QueryFields)": func(q *gorm.DB) *gorm.DB {
			return q.Session(&gorm.Session{QueryFields: true})
		},
	}
	changes := map[string]func(q *gorm.DB){
		"branch": func(q *gorm.DB) { q.Where("active = ?", true).Find(&[]User{}) },
		"reassignment": func(q *gorm.DB) {
			q = q.Where("active = ?", true)
			q.Find(&[]User{})
		},
	}
	for copyName, takeCopy := range copies {
		for changeName, change := range changes {
			t.Run(copyName+"/"+changeName, func(t *testing.T) {
				db, captured := setupDB(t)

				q := db.Model(&User{}).Where("tenant = ?", 1) // clone==0
				countQ := takeCopy(q)
				change(q)
				countQ.Count(new(int64))

				if len(*captured) < 2 {
					t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
				}
				if last := (*captured)[len(*captured)-1]; !strings.Contains(last, "active") {
					t.Errorf("copy taken before the change should see it (shared Statement): %s", last)
				}
			})
		}
	}
}

// TestCopyOfMidChain_UseDoesNotChangeOriginal asserts the other direction: using
// the copy forks its own Statement, so the original is unchanged. This is why a
// copy used before the original changes is safe.
func TestCopyOfMidChain_UseDoesNotChangeOriginal(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("tenant = ?", 1) // clone==0
	countQ := q.Session(&gorm.Session{})
	countQ.Where("copy_only = ?", 1).Count(new(int64))
	q.Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if last := (*captured)[len(*captured)-1]; strings.Contains(last, "copy_only") {
		t.Errorf("using the copy must not change the original: %s", last)
	}
}

// TestCopyOfMidChain_IsolatingConfigsDoNotLeak asserts that WithContext, and
// Session with Context, PrepareStmt, SkipHooks or NewDB, do not share the
// Statement: a later change to the original does not show up in the copy.
func TestCopyOfMidChain_IsolatingConfigsDoNotLeak(t *testing.T) {
	ctx := context.Background()
	copies := map[string]func(q *gorm.DB) *gorm.DB{
		"WithContext":        func(q *gorm.DB) *gorm.DB { return q.WithContext(ctx) },
		"Session(Context)":   func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{Context: ctx}) },
		"Session(SkipHooks)": func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{SkipHooks: true}) },
		"Session(NewDB)":     func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{NewDB: true}) },
	}
	for name, takeCopy := range copies {
		t.Run(name, func(t *testing.T) {
			db, captured := setupDB(t)

			q := db.Model(&User{}).Where("tenant = ?", 1) // clone==0
			countQ := takeCopy(q)
			q.Where("active = ?", true).Find(&[]User{})
			countQ.Model(&User{}).Count(new(int64))

			if len(*captured) < 2 {
				t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
			}
			if last := (*captured)[len(*captured)-1]; strings.Contains(last, "active") {
				t.Errorf("%s must not share the Statement: %s", name, last)
			}
		})
	}
}

// TestSessionInitialized_ReturnsMidChain asserts that
// Session(&gorm.Session{Initialized: true}) returns a clone==0 value: branching
// it twice accumulates conditions, even on the root DB.
func TestSessionInitialized_ReturnsMidChain(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Session(&gorm.Session{Initialized: true})
	q.Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	second := (*captured)[1]
	if !strings.Contains(second, "a =") || !strings.Contains(second, "b =") {
		t.Errorf("Initialized result should be mid-chain and accumulate both 'a' and 'b': %s", second)
	}
}

// TestSessionInitialized_SnapshotsReceiver asserts that the Initialized result
// has its own copy of the receiver's Statement: a later change to the receiver
// does not show up in it.
func TestSessionInitialized_SnapshotsReceiver(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("tenant = ?", 1) // clone==0
	init := q.Session(&gorm.Session{Initialized: true})
	q.Where("active = ?", true).Find(&[]User{})
	init.Count(new(int64))

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if last := (*captured)[len(*captured)-1]; strings.Contains(last, "active") {
		t.Errorf("Initialized result must not share the receiver's Statement: %s", last)
	}
}

// TestRangeOverFuncReuseAccumulates asserts that a range-over-func loop body
// runs once per element, so a mid-chain (clone==0) value branched inside it
// accumulates conditions across iterations, as in a plain loop. This is why
// the analyzer treats the body as a loop (#177).
func TestRangeOverFuncReuseAccumulates(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1) // clone==0
	for v := range slices.Values([]int{10, 20}) {
		q.Where("age = ?", v).Find(&[]User{})
	}

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if count := strings.Count((*captured)[1], "age ="); count != 2 {
		t.Errorf("second iteration should accumulate the first iteration's 'age' condition (got %d): %s", count, (*captured)[1])
	}
}

// pureReturnArg does not pollute db, but returns it (#188).
func pureReturnArg(db *gorm.DB) *gorm.DB { return db }

// pureWithTenant does not pollute db, and isolates its result with Session.
func pureWithTenant(db *gorm.DB, id int) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", id)
}

// TestPureReturnedArgShares asserts that a branch from the result of a function
// that returns its argument is a branch from the argument: the second query
// inherits the first one's condition. This is why the analyzer traces such a
// result to the argument's root (#188).
func TestPureReturnedArgShares(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1) // clone==0
	pureReturnArg(q).Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; !strings.Contains(second, "a =") {
		t.Errorf("the result is the argument, so the second query should accumulate 'a': %s", second)
	}
}

// TestPureSessionResultIsolated asserts that a result built from
// db.Session(...).Where(...) does not share the argument's Statement, so the
// analyzer keeps it as its own root (#188).
func TestPureSessionResultIsolated(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1) // clone==0
	pureWithTenant(q, 7).Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; strings.Contains(second, "a =") || strings.Contains(second, "tenant_id") {
		t.Errorf("the Session-isolated result must not leak into the argument: %s", second)
	}
}

// TestMethodValue_ImmutableReceiverIsSafe asserts that each call through a
// method value of a clone>0 handle forks a fresh Statement, like calling the
// method directly (#178): w("a") and w("b") do not see each other.
func TestMethodValue_ImmutableReceiverIsSafe(t *testing.T) {
	db, captured := setupDB(t)

	w := db.Session(&gorm.Session{}).Where
	w("a = ?", 10).Find(&[]User{})
	w("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; strings.Contains(second, "a =") {
		t.Errorf("each call through the method value should fork a fresh Statement: %s", second)
	}
}

// TestMethodValue_MutableReceiverAccumulates asserts that a method value of a
// mid-chain (clone==0) handle changes that handle's Statement on each call, so
// the second call is a second branch of the receiver.
func TestMethodValue_MutableReceiverAccumulates(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("tenant = ?", 1) // clone==0
	w := q.Where
	w("a = ?", 10).Find(&[]User{})
	w("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; !strings.Contains(second, "a =") || !strings.Contains(second, "b =") {
		t.Errorf("calls through a mid-chain method value should accumulate both 'a' and 'b': %s", second)
	}
}

// TestMethodValue_SessionCopySharesStatement asserts that a Session copy taken
// through a method value (s := q.Session) shares q's Statement like
// q.Session(...) does.
func TestMethodValue_SessionCopySharesStatement(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("tenant = ?", 1) // clone==0
	s := q.Session
	countQ := s(&gorm.Session{})
	q.Where("active = ?", true).Find(&[]User{})
	countQ.Count(new(int64))

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if last := (*captured)[len(*captured)-1]; !strings.Contains(last, "active") {
		t.Errorf("a copy taken through a method value should see the later change: %s", last)
	}
}
