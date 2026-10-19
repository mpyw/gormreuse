package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// Fields of a struct returned by value and by pointer, and a struct built
// inside a loop (#250).

func makeUsersQueryPtr(db *gorm.DB) *usersQuery {
	return &usersQuery{Users: db.Model(&User{}).Where("tenant = ?", 1)}
}

// TestReturnedPointerStructFieldAccumulate: a field reached through a
// returned pointer accumulates conditions like one of a returned value, so
// the analyzer reports both.
func TestReturnedPointerStructFieldAccumulate(t *testing.T) {
	db, captured := setupDB(t)

	s := makeUsersQueryPtr(db)
	s.Users.Where("a = ?", 10).Find(&[]User{})
	s.Users.Where("b = ?", 20).Find(&[]User{})

	assertSecondAccumulates(t, *captured)
}

// TestReturnedStructInLoopIsFresh: a struct the call builds on each iteration
// holds a new chain, so no iteration sees an earlier one's condition. The
// analyzer must not report it.
func TestReturnedStructInLoopIsFresh(t *testing.T) {
	db, captured := setupDB(t)

	for _, name := range []string{"alice", "bob"} {
		s := makeUsersQuery(db)
		s.Users.Where("name = ?", name).Find(&[]User{})
	}

	if len(*captured) < 2 {
		t.Fatalf("Expected 2 queries, got %d: %v", len(*captured), *captured)
	}
	if second := (*captured)[1]; strings.Count(second, "name =") != 1 {
		t.Errorf("the second iteration should not see the first iteration's condition: %s", second)
	}
}
