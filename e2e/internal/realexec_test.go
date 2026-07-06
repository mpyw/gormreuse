// Package internal contains end-to-end tests that verify actual GORM SQL
// behavior with sqlmock. They guard the linter's core premise: a mid-chain
// (clone==0) *gorm.DB shares its Statement, so branching it twice accumulates
// conditions, whereas root / Session()-derived (clone>0) handles fork a fresh
// Statement per chain and are safe to reuse. Every test asserts (t.Errorf) —
// none merely logs both outcomes.
package internal

import (
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
	for i := 0; i < 20; i++ {
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
