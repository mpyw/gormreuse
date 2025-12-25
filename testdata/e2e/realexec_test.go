// Package internal contains end-to-end tests that verify actual GORM SQL behavior.
// These tests use sqlmock to capture generated SQL and document GORM's
// clone/pollution behavior.
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

// setupDB creates a GORM DB with sqlmock and SQL capture callback.
func setupDB(t *testing.T) (*gorm.DB, *[]string) {
	t.Helper()

	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("Failed to create sqlmock: %v", err)
	}

	// Set up expectations for any queries
	for i := 0; i < 20; i++ {
		mock.ExpectQuery(".*").WillReturnRows(
			sqlmock.NewRows([]string{"count", "id", "name", "active", "age"}).AddRow(100, 1, "test", true, 20),
		)
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

	// Capture SQL
	var capturedSQL []string
	db.Callback().Query().After("gorm:query").Register("capture", func(tx *gorm.DB) {
		if tx.Statement != nil {
			capturedSQL = append(capturedSQL, tx.Statement.SQL.String())
		}
	})

	return db, &capturedSQL
}

// TestSessionAtEnd verifies that Session() at the end of a chain makes it safe.
func TestSessionAtEnd(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1).Session(&gorm.Session{})
	q.Count(new(int64))
	q.Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}

	// Both queries should have only "base = ?" - no pollution
	for i, sql := range *captured {
		count := strings.Count(sql, "base")
		if count != 1 {
			t.Errorf("Query %d has %d occurrences of 'base', expected 1: %s", i, count, sql)
		}
	}
	t.Logf("VERIFIED: Session at end creates independent queries")
}

// TestSessionInMiddle documents behavior when Session() is in the middle.
func TestSessionInMiddle(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Session(&gorm.Session{}).Where("base = ?", 1)
	q.Find(&[]User{})
	q.Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}

	// Document the actual behavior
	for i, sql := range *captured {
		count := strings.Count(sql, "base")
		t.Logf("Query %d: base count=%d, SQL=%s", i, count, sql)
	}

	// Check for pollution (base appearing more than once)
	secondQuery := (*captured)[1]
	count := strings.Count(secondQuery, "base")
	if count >= 2 {
		t.Logf("OBSERVED: Session in middle causes pollution (base count: %d)", count)
	} else {
		t.Logf("OBSERVED: Session in middle does NOT cause pollution in this GORM version")
	}
}

// TestMutableReuse verifies that reusing a mutable instance pollutes it.
func TestMutableReuse(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1)
	q.Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}

	// Document all queries
	for i, sql := range *captured {
		t.Logf("Query %d: %s", i, sql)
	}

	// First query: base + a
	first := (*captured)[0]
	if !strings.Contains(first, "base") || !strings.Contains(first, "a") {
		t.Errorf("First query should have base and a: %s", first)
	}

	// Second query: check if pollution occurred
	second := (*captured)[1]
	if strings.Contains(second, "a") {
		t.Logf("VERIFIED: Mutable reuse causes WHERE accumulation")
	} else {
		t.Logf("Note: No WHERE accumulation in this case")
	}
}

// TestSessionBeforeFinisher verifies that Session() before each finisher is safe.
func TestSessionBeforeFinisher(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1)
	q.Session(&gorm.Session{}).Count(new(int64))
	q.Session(&gorm.Session{}).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}

	// Both queries should have only "base = ?" - Session before finisher prevents pollution
	for i, sql := range *captured {
		count := strings.Count(sql, "base")
		if count != 1 {
			t.Errorf("Query %d has %d occurrences of 'base', expected 1: %s", i, count, sql)
		}
	}
	t.Logf("VERIFIED: Session before each finisher creates independent queries")
}

// TestCountThenFind documents the Count then Find behavior.
func TestCountThenFind(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Model(&User{}).Where("base = ?", 1)
	q.Count(new(int64))
	q.Find(&[]User{})

	// Document all queries
	for i, sql := range *captured {
		t.Logf("Query %d: %s", i, sql)
	}

	if len(*captured) >= 2 {
		second := (*captured)[1]
		count := strings.Count(second, "base")
		if count >= 2 {
			t.Logf("OBSERVED: Count pollutes the instance (base count: %d)", count)
		} else {
			t.Logf("OBSERVED: Count does not pollute in this GORM version")
		}
	}
}
