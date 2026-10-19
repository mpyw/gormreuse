package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// These tests pin the runtime premises of the parameter-root and Phi-edge
// fixes (#201).

// paramSessionFixed is what -fix makes of a function that branches its
// parameter twice when a caller may pass a mid-chain value: a Session at the
// top of the body.
func paramSessionFixed(tx *gorm.DB) {
	tx = tx.Session(&gorm.Session{})
	tx.Where("a = ?", 10).Find(&[]User{})
	tx.Where("b = ?", 20).Find(&[]User{})
}

// TestParamSessionIsolatesBranches: with the Session in the body, a mid-chain
// argument no longer leaks the first branch into the second, and both keep
// the caller's condition.
func TestParamSessionIsolatesBranches(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, true)

	paramSessionFixed(db.Model(&User{}).Where("x = ?", 1))
	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	first, second := (*captured)[0], (*captured)[1]
	if !strings.Contains(first, "x =") || !strings.Contains(second, "x =") {
		t.Errorf("both queries should keep the caller's condition:\n%s\n%s", first, second)
	}
	if strings.Contains(second, "a =") {
		t.Errorf("the second branch should not see the first: %s", second)
	}
}

// TestSessionMidChainEdgeAccumulates: a value whose last call is a chain
// method is mutable even when a Session comes earlier in the chain, so a Phi
// edge such as db.Session(s).Where("y") needs a Session of its own.
func TestSessionMidChainEdgeAccumulates(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, true)

	q := db.Session(&gorm.Session{}).Model(&User{}).Where("y = ?", 2)
	q.Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})
	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; !strings.Contains(second, "a =") {
		t.Errorf("the second branch should have ACCUMULATED 'a': %s", second)
	}
}
