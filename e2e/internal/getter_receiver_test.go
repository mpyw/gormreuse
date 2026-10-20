package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// getterHolder's getter returns the field of its receiver. gormreuse infers
// such a getter immutable-return, because a field read through a receiver is
// immutable where it is read (#308). #342 asserts that this holds only where
// the receiver holds an immutable value: a getter hands back exactly what the
// struct holds, so a struct holding a mid-chain value hands back that chain.
// (This package assigns the field provable chains, so here the getter is not
// inferred at all.)
type getterHolder struct{ db *gorm.DB }

func (h *getterHolder) DB() *gorm.DB { return h.db }

// getterTenant branches what the getter returns, on a struct holding its
// argument. It is the shape of the pure function in #342.
func getterTenant(q *gorm.DB) {
	h := &getterHolder{db: q}
	h.DB().Where("tenant = ?", 1)
}

// TestGetterOfMidChainReceiver shows that branching a getter's result
// pollutes the chain the receiver holds: the condition added through the
// getter leaks into the caller's next query on the same chain.
func TestGetterOfMidChainReceiver(t *testing.T) {
	db, captured := setupDB(t)

	q := db.Session(&gorm.Session{}).Model(&User{}).Where("a = ?", 1)
	getterTenant(q)
	q.Find(&[]User{})

	if len(*captured) < 1 {
		t.Fatalf("Expected a query, got %d", len(*captured))
	}
	if got := (*captured)[0]; !strings.Contains(got, "tenant =") {
		t.Errorf("the getter returns the receiver's mid-chain value, so its branch should leak 'tenant' into q: %s", got)
	}
}

// TestGetterOfMidChainReused shows the second shape of #342: the getter's
// result, branched twice, accumulates conditions like the chain itself.
func TestGetterOfMidChainReused(t *testing.T) {
	db, captured := setupDB(t)

	h := &getterHolder{db: db.Session(&gorm.Session{}).Model(&User{}).Where("base = ?", 1)}
	d := h.DB()
	d.Where("a = ?", 10).Find(&[]User{})
	d.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; !strings.Contains(second, "a =") {
		t.Errorf("the getter's result is clone==0, so the second branch should inherit 'a': %s", second)
	}
}

// TestGetterOfIsolatedReceiver is the safe case: a struct holding a Session
// result hands back a clone>0 value, which can be branched freely.
func TestGetterOfIsolatedReceiver(t *testing.T) {
	db, captured := setupDB(t)

	h := &getterHolder{db: db.Model(&User{}).Where("base = ?", 1).Session(&gorm.Session{})}
	d := h.DB()
	d.Where("a = ?", 10).Find(&[]User{})
	d.Where("b = ?", 20).Find(&[]User{})

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; strings.Contains(second, "a =") {
		t.Errorf("the getter's result is clone>0, so the second branch should not inherit 'a': %s", second)
	}
}
