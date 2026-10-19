package internal

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// TestInferredImmutableReturnIsolates asserts the runtime premise of #261: a
// helper that returns a Session, WithContext or Debug result, alone or in a
// (*gorm.DB, error) result, hands back a clone>0 value, even when its argument
// is mid-chain. Branching that result twice does not accumulate conditions,
// so the analyzer may infer immutable-return for such a helper.
func TestInferredImmutableReturnIsolates(t *testing.T) {
	tests := []struct {
		name string
		get  func(db *gorm.DB) *gorm.DB
	}{
		{"Session", inferredSessionHelper},
		{"WithContext", inferredWithContextHelper},
		{"Debug", inferredDebugHelper},
		{"tuple", func(db *gorm.DB) *gorm.DB {
			q, _ := inferredTupleHelper(db)
			return q
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, captured := setupDB(t)

			q := tt.get(db.Model(&User{}).Where("base = ?", 1))
			q.Where("a = ?", 10).Find(&[]User{})
			q.Where("b = ?", 20).Find(&[]User{})

			if len(*captured) < 2 {
				t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
			}
			second := (*captured)[1]
			if strings.Contains(second, "a =") {
				t.Errorf("the result is clone>0, so the second branch should not inherit 'a': %s", second)
			}
			if !strings.Contains(second, "base =") {
				t.Errorf("the second branch should keep the helper's argument chain: %s", second)
			}
		})
	}
}

func inferredSessionHelper(db *gorm.DB) *gorm.DB { return db.Session(&gorm.Session{}) }

func inferredWithContextHelper(db *gorm.DB) *gorm.DB { return db.WithContext(context.Background()) }

func inferredDebugHelper(db *gorm.DB) *gorm.DB { return db.Debug() }

func inferredTupleHelper(db *gorm.DB) (*gorm.DB, error) { return inferredSessionHelper(db), nil }
