package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// namedTxCallback branches its parameter twice. As a Transaction callback it
// receives a fresh tx; called directly it receives whatever the caller passes.
func namedTxCallback(q *gorm.DB) error {
	q.Where("a = ?", 10).Find(&[]User{})
	q.Where("b = ?", 20).Find(&[]User{})
	return nil
}

// TestNamedCallbackDirectCallAccumulates asserts the runtime premise of #191:
// the same named function is safe as a Transaction callback but leaks the
// first branch into the second when called directly with a mid-chain value.
// So passing a function to Transaction cannot exempt its parameter for every
// caller.
func TestNamedCallbackDirectCallAccumulates(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, true)

	if err := db.Model(&User{}).Transaction(namedTxCallback); err != nil {
		t.Fatalf("Transaction returned error: %v", err)
	}
	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; strings.Contains(second, "a =") {
		t.Errorf("as a Transaction callback the branches must be independent: %s", second)
	}

	*captured = nil
	_ = namedTxCallback(db.Model(&User{}).Where("x = ?", 1))
	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; !strings.Contains(second, "a =") {
		t.Errorf("called directly with a mid-chain value the second branch should have ACCUMULATED 'a': %s", second)
	}
}
