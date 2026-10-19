package internal

import (
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
)

// branchTxTwice branches tx twice, as the fixtures in
// testdata/src/gormreuse/callback_go_defer.go do.
func branchTxTwice(tx *gorm.DB) error {
	tx.Model(&User{}).Where("a = ?", 10).Find(&[]User{})
	tx.Model(&User{}).Where("b = ?", 20).Find(&[]User{})
	return nil
}

// TestCallbackGoDeferTransactionIsolated asserts the runtime premise of #256:
// a Transaction called under defer or go still hands its callback a fresh tx,
// so the callback's two branches stay independent.
func TestCallbackGoDeferTransactionIsolated(t *testing.T) {
	tests := map[string]func(db *gorm.DB){
		"defer": func(db *gorm.DB) {
			defer db.Where("x = ?", 1).Transaction(branchTxTwice) //nolint:errcheck // the deferred form under test cannot keep the error
		},
		"go": func(db *gorm.DB) {
			var wg sync.WaitGroup
			wg.Add(1)
			go db.Where("x = ?", 1).Transaction(func(tx *gorm.DB) error { //nolint:errcheck // the go form under test cannot keep the error
				defer wg.Done()
				return branchTxTwice(tx)
			})
			wg.Wait()
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			db, captured, _ := setupDBWithMock(t, true)
			run(db)
			if len(*captured) < 2 {
				t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
			}
			if second := (*captured)[1]; strings.Contains(second, "a =") {
				t.Errorf("the callback's branches must be independent: second query leaked 'a': %s", second)
			}
		})
	}
}
