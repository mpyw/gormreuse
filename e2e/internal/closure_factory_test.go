package internal

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// These tests pin the runtime premises of #251: a closure that builds a chain
// from an immutable handle starts a new Statement on each call, and a captured
// parameter rebound with WithContext no longer shares the caller's Statement.

// TestClosureFactoryCallsAreIndependent: each call of a factory over an
// immutable db builds its own chain, called twice or in a loop.
func TestClosureFactoryCallsAreIndependent(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, false)

	db = db.Session(&gorm.Session{})
	base := func() *gorm.DB { return db.Model(&User{}) }
	base().Where("c = ?", 1).Find(&[]User{})
	base().Where("d = ?", 2).Find(&[]User{})

	q := func(id int) *gorm.DB { return db.Where("id = ?", id) }
	for _, id := range []int{3, 4} {
		q(id).Find(&[]User{})
	}

	if len(*captured) < 4 {
		t.Fatalf("Expected at least 4 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; strings.Contains(second, "c =") {
		t.Errorf("the second factory call should not see the first: %s", second)
	}
	if last := (*captured)[3]; strings.Count(last, "id =") != 1 {
		t.Errorf("each loop call should have one id condition: %s", last)
	}
}

// TestClosureFactoryOverMutableRootAccumulates: a factory over a captured
// mid-chain value branches that value on every call, which stays reported.
func TestClosureFactoryOverMutableRootAccumulates(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, false)

	q := db.Model(&User{}).Where("x = ?", 1)
	f := func() *gorm.DB { return q.Where("y = ?", 2) }
	f().Find(&[]User{})
	f().Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; strings.Count(second, "y =") < 2 {
		t.Errorf("the second call should have ACCUMULATED 'y': %s", second)
	}
}

// closureRebind rebinds its parameter before a closure reads it.
func closureRebind(ctx context.Context, db *gorm.DB) {
	db = db.WithContext(ctx)
	db.Where("a = ?", 10).Find(&[]User{})
	func() { db.Where("b = ?", 20).Find(&[]User{}) }()
}

// TestClosureRebindIsolatesBranches: after db = db.WithContext(ctx), the
// closure's branch does not see the first one, even for a mid-chain argument,
// and both keep the caller's condition.
func TestClosureRebindIsolatesBranches(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, false)

	closureRebind(context.Background(), db.Model(&User{}).Where("x = ?", 1))

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	first, second := (*captured)[0], (*captured)[1]
	if !strings.Contains(first, "x =") || !strings.Contains(second, "x =") {
		t.Errorf("both queries should keep the caller's condition:\n%s\n%s", first, second)
	}
	if strings.Contains(second, "a =") {
		t.Errorf("the closure's branch should not see the first: %s", second)
	}
}
