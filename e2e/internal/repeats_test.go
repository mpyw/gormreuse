package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// The runtime premises of testdata/src/gormreuse/repeats.go (#249, #252): a
// use that runs more than once for one definition of its mutable root reuses
// the root, even with one use in the source. Each run adds its condition to
// the root's Statement, so the second query also has the first one's
// condition. A Session on the root, as the fix suggests, keeps the queries
// apart.

// repeatsGroup runs each function at once, as an errgroup.Group whose
// goroutines happen to run in order would. It keeps the test free of races.
type repeatsGroup struct{}

func (repeatsGroup) Go(f func() error) { _ = f() }

// repeatsCases builds each repro of #249 and #252 on a root that session
// makes immutable or leaves mutable. Each runs the use "a = ?" twice.
func repeatsCases(session bool) map[string]func(db *gorm.DB) {
	root := func(db *gorm.DB) *gorm.DB {
		q := db.Model(&User{}).Where("x = ?", 1) // clone==0
		if session {
			q = q.Session(&gorm.Session{})
		}
		return q
	}
	ids := []int{1, 2}
	return map[string]func(db *gorm.DB){
		"closure called twice": func(db *gorm.DB) {
			q := root(db)
			f := func() { q.Where("a = ?", 1).Find(&[]User{}) }
			f()
			f()
		},
		"closure called in a loop": func(db *gorm.DB) {
			q := root(db)
			f := func() { q.Where("a = ?", 1).Find(&[]User{}) }
			for range ids {
				f()
			}
		},
		"IIFE in a loop": func(db *gorm.DB) {
			q := root(db)
			for range ids {
				func() { q.Where("a = ?", 1).Find(&[]User{}) }()
			}
		},
		"closure passed twice": func(db *gorm.DB) {
			q := root(db)
			var g repeatsGroup
			f := func() error { return q.Where("a = ?", 1).Find(&[]User{}).Error }
			g.Go(f)
			g.Go(f)
		},
		"closure passed in a loop": func(db *gorm.DB) {
			q := root(db)
			var g repeatsGroup
			for _, id := range ids {
				g.Go(func() error { return q.Where("a = ?", id).Find(&[]User{}).Error })
			}
		},
		"defer in a loop": func(db *gorm.DB) {
			q := root(db)
			for range ids {
				defer q.Where("a = ?", 1).Find(&[]User{})
			}
		},
		"use before an inner loop": func(db *gorm.DB) {
			q := root(db)
			for i := range 3 {
				if i > 0 {
					q.Where("a = ?", 1).Find(&[]User{})
				}
				for range 2 {
				}
			}
		},
		"root of the outer loop used in the inner loop": func(db *gorm.DB) {
			for range 1 {
				q := root(db)
				for range ids {
					q.Where("a = ?", 1).Find(&[]User{})
				}
			}
		},
	}
}

// TestRepeatsReuseLeaks asserts that the second run of each use sees the
// first run's condition on a mutable root.
func TestRepeatsReuseLeaks(t *testing.T) {
	for name, run := range repeatsCases(false) {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, run)
			if len(got) != 2 {
				t.Fatalf("Expected 2 queries, got %d: %v", len(got), got)
			}
			if n := strings.Count(got[1], "a = ?"); n != 2 {
				t.Errorf("the second run should repeat the first one's condition, got %d: %s", n, got[1])
			}
		})
	}
}

// TestRepeatsSessionIsolates asserts that a Session on the root, the
// suggested fix, gives each run only its own condition.
func TestRepeatsSessionIsolates(t *testing.T) {
	for name, run := range repeatsCases(true) {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, run)
			subqueryWantSQL(t, got, []string{"a = ? AND a = ?"}, "x = ? AND a = ?", "x = ? AND a = ?")
		})
	}
}
