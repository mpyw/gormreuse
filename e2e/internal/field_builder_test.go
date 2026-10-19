package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// userRepo holds its query in a field and narrows it with pointer-receiver
// builder methods, the pattern of #182.
type userRepo struct{ db *gorm.DB }

func (r *userRepo) whereTenant() { r.db = r.db.Where("tenant = ?", 1) }

// TestFieldBuilderSingleUse asserts that `r.db = r.db.Where(...)` followed by
// one use is safe: the query has each condition once. The analyzer must not
// report it (#182).
func TestFieldBuilderSingleUse(t *testing.T) {
	db, captured := setupDB(t)

	r := &userRepo{db: db.Model(&User{})}
	r.whereTenant()
	r.db.Where("a = ?", 10).Find(&[]User{})

	if len(*captured) != 1 {
		t.Fatalf("Expected 1 query, got %d: %v", len(*captured), *captured)
	}
	sql := (*captured)[0]
	if strings.Count(sql, "tenant =") != 1 || strings.Count(sql, "a =") != 1 {
		t.Errorf("query should have 'tenant' and 'a' once each: %s", sql)
	}
}

// TestFieldBuilderLoopSingleUse asserts that rebuilding the field in a loop and
// using it once afterwards is safe: each condition appears once (#182).
func TestFieldBuilderLoopSingleUse(t *testing.T) {
	db, captured := setupDB(t)

	r := &userRepo{db: db.Model(&User{})}
	for _, c := range []string{"a = ?", "b = ?"} {
		r.db = r.db.Where(c, 1)
	}
	r.db.Find(&[]User{})

	if len(*captured) != 1 {
		t.Fatalf("Expected 1 query, got %d: %v", len(*captured), *captured)
	}
	sql := (*captured)[0]
	if strings.Count(sql, "a =") != 1 || strings.Count(sql, "b =") != 1 {
		t.Errorf("query should have 'a' and 'b' once each: %s", sql)
	}
}

// TestFieldBuilderTwoUsesAccumulate asserts that two branches from the value
// the builder stored do interfere, which is why the analyzer still reports the
// second one.
func TestFieldBuilderTwoUsesAccumulate(t *testing.T) {
	db, captured := setupDB(t)

	r := &userRepo{db: db.Model(&User{})}
	r.whereTenant()
	r.db.Where("a = ?", 10).Find(&[]User{})
	r.db.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; !strings.Contains(second, "a =") {
		t.Errorf("the second branch should have ACCUMULATED 'a' from the first: %s", second)
	}
}
