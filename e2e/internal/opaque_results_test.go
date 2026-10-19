package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// TestOpaqueResultReuseAccumulates asserts the runtime premise of #176: a
// helper with a (*gorm.DB, error) result, a func-typed value and an interface
// method can each hand back a clone==0 value, so branching that result twice
// accumulates conditions exactly as branching a single-result helper's does.
func TestOpaqueResultReuseAccumulates(t *testing.T) {
	var r querier = baseQuerier{}
	tests := []struct {
		name string
		get  func(db *gorm.DB) *gorm.DB
	}{
		{"tuple helper", func(db *gorm.DB) *gorm.DB {
			q, _ := tupleWhereBase(db)
			return q
		}},
		{"func-typed value", whereBase},
		{"interface method", r.Query},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, captured := setupDB(t)

			q := tt.get(db.Model(&User{}).Session(&gorm.Session{}))
			q.Where("a = ?", 10).Find(&[]User{})
			q.Where("b = ?", 20).Find(&[]User{})

			if len(*captured) < 2 {
				t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
			}
			if second := (*captured)[1]; !strings.Contains(second, "a =") {
				t.Errorf("the result is clone==0, so the second branch should inherit 'a': %s", second)
			}
		})
	}
}

func whereBase(db *gorm.DB) *gorm.DB { return db.Where("base = ?", 1) }

func tupleWhereBase(db *gorm.DB) (*gorm.DB, error) { return whereBase(db), nil }

type querier interface{ Query(db *gorm.DB) *gorm.DB }

type baseQuerier struct{}

func (baseQuerier) Query(db *gorm.DB) *gorm.DB { return whereBase(db) }
