package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// The struct shapes of #181. In each one the second branch of a mutable chain
// held in a struct accumulates the condition of the first, so the analyzer
// must report it.

type embeddedQuery struct{ *gorm.DB }

type usersQuery struct{ Users *gorm.DB }

func makeUsersQuery(db *gorm.DB) usersQuery {
	return usersQuery{Users: db.Model(&User{}).Where("tenant = ?", 1)}
}

type queryHolder struct{ q *gorm.DB }

// assertSecondAccumulates asserts that the second captured query also has the
// condition of the first branch.
func assertSecondAccumulates(t *testing.T, captured []string) {
	t.Helper()
	if len(captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d: %v", len(captured), captured)
	}
	if second := captured[1]; !strings.Contains(second, "a =") {
		t.Errorf("the second branch should have ACCUMULATED 'a' from the first: %s", second)
	}
}

// TestEmbeddedPromotedMethodsAccumulate covers methods promoted from an
// embedded *gorm.DB.
func TestEmbeddedPromotedMethodsAccumulate(t *testing.T) {
	db, captured := setupDB(t)

	e := embeddedQuery{db.Model(&User{}).Where("tenant = ?", 1)}
	e.Where("a = ?", 10).Find(&[]User{})
	e.Where("b = ?", 20).Find(&[]User{})

	assertSecondAccumulates(t, *captured)
}

// TestReturnedStructFieldAccumulate covers a field of a struct returned by a
// call.
func TestReturnedStructFieldAccumulate(t *testing.T) {
	db, captured := setupDB(t)

	s := makeUsersQuery(db)
	s.Users.Where("a = ?", 10).Find(&[]User{})
	s.Users.Where("b = ?", 20).Find(&[]User{})

	assertSecondAccumulates(t, *captured)
}

// TestClosureCapturedStructAccumulate covers a closure that captures a struct
// holding the chain.
func TestClosureCapturedStructAccumulate(t *testing.T) {
	db, captured := setupDB(t)

	h := queryHolder{q: db.Model(&User{}).Where("tenant = ?", 1)}
	h.q.Where("a = ?", 10).Find(&[]User{})
	f := func() { h.q.Where("b = ?", 20).Find(&[]User{}) }
	f()

	assertSecondAccumulates(t, *captured)
}
