package internal

import (
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
)

// The runtime premises of testdata/src/gormreuse/repeats_closure_sites.go
// (#305, #306).
//
//   - A closure that another closure calls through a captured variable runs
//     each time that closure calls it, so a mutable root it captures gets the
//     condition of every run.
//   - A closure made in a loop body is a new closure, over a new root, on each
//     iteration: calls of it on exclusive branches do not accumulate.
//   - A use in a loop reuses every root that a merge may hold. A Session on
//     the first root alone leaves the other one accumulating.

// closureSitesCases builds each #305 repro on a root that session makes
// immutable or leaves mutable. Each runs "id = ?" three times.
func closureSitesCases(session bool) map[string]func(db *gorm.DB) {
	root := func(db *gorm.DB) *gorm.DB {
		q := db.Model(&User{}).Where("name = ?", "x") // clone==0
		if session {
			q = q.Session(&gorm.Session{})
		}
		return q
	}
	ids := []int{1, 2, 3}
	return map[string]func(db *gorm.DB){
		"called in a loop of another closure": func(db *gorm.DB) {
			base := root(db)
			update := func(id int) { base.Where("id = ?", id).Find(&[]User{}) }
			run := func() {
				for _, id := range ids {
					update(id)
				}
			}
			run()
		},
		"called three times in an IIFE": func(db *gorm.DB) {
			base := root(db)
			update := func(id int) { base.Where("id = ?", id).Find(&[]User{}) }
			func() {
				update(1)
				update(2)
				update(3)
			}()
		},
		"called in a loop of a goroutine": func(db *gorm.DB) {
			base := root(db)
			update := func(id int) { base.Where("id = ?", id).Find(&[]User{}) }
			var wg sync.WaitGroup
			wg.Go(func() {
				for _, id := range ids {
					update(id)
				}
			})
			wg.Wait()
		},
	}
}

// TestClosureSitesThroughClosureAccumulate asserts that each run of a
// closure called through another closure adds its condition to the captured
// mutable root (#305).
func TestClosureSitesThroughClosureAccumulate(t *testing.T) {
	for name, run := range closureSitesCases(false) {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, run)
			if len(got) != 3 {
				t.Fatalf("Expected 3 queries, got %d: %v", len(got), got)
			}
			for i, sql := range got {
				if n := strings.Count(sql, "id = ?"); n != i+1 {
					t.Errorf("query %d should carry %d id conditions, got %d: %s", i, i+1, n, sql)
				}
			}
		})
	}
}

// TestClosureSitesThroughClosureSessionIsolates asserts that a Session on
// the root, the suggested fix, gives each run only its own condition.
func TestClosureSitesThroughClosureSessionIsolates(t *testing.T) {
	for name, run := range closureSitesCases(true) {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, run)
			subqueryWantSQL(t, got, []string{"id = ? AND id = ?"}, "name = ? AND id = ?", "name = ? AND id = ?", "name = ? AND id = ?")
		})
	}
}

// TestClosureSitesThroughTransactionAccumulate is the Transaction callback
// case of #305: the callback calls the closure twice.
func TestClosureSitesThroughTransactionAccumulate(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, true)
	base := db.Model(&User{}).Where("name = ?", "x")
	update := func(id int) error { return base.Where("id = ?", id).Find(&[]User{}).Error }
	if err := db.Transaction(func(*gorm.DB) error {
		_ = update(1)
		return update(2)
	}); err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	got := *captured
	if len(got) != 2 {
		t.Fatalf("Expected 2 queries, got %d: %v", len(got), got)
	}
	if n := strings.Count(got[1], "id = ?"); n != 2 {
		t.Errorf("the second call should repeat the first one's condition, got %d: %s", n, got[1])
	}
}

// TestClosureSitesPerIterationExclusiveIsolated asserts that a closure made
// in a loop body and called on exclusive branches runs once per root (#306).
func TestClosureSitesPerIterationExclusiveIsolated(t *testing.T) {
	type item struct {
		ID  int
		New bool
	}
	items := []item{{1, true}, {2, false}, {3, true}}
	got := subqueryOuterSQL(t, func(db *gorm.DB) {
		for _, it := range items {
			q := db.Model(&User{}).Where("id = ?", it.ID)
			save := func() { q.Find(&[]User{}) }
			if it.New {
				save()
			} else {
				save()
			}
		}
	})
	subqueryWantSQL(t, got, []string{"id = ? AND id = ?"}, "id = ?", "id = ?", "id = ?")
}

// closureSitesMerged runs the merged-roots repro of #306: q is the Model root
// or, with org, the Where root, then used on every iteration. sessionModel
// and sessionWhere put a Session on each root, as the fix does.
func closureSitesMerged(org *string, sessionModel, sessionWhere bool) func(db *gorm.DB) {
	return func(db *gorm.DB) {
		q := db.Model(&User{})
		if sessionModel {
			q = q.Session(&gorm.Session{})
		}
		if org != nil {
			q = q.Where("org = ?", *org)
			if sessionWhere {
				q = q.Session(&gorm.Session{})
			}
		}
		for _, id := range []int{1, 2} {
			q.Where("id = ?", id).Find(&[]User{})
		}
	}
}

// TestClosureSitesMergedRootsAccumulate asserts that both roots of the merge
// accumulate in the loop, and that a Session on the Model root alone, the fix
// before #306, still leaves the Where root accumulating.
func TestClosureSitesMergedRootsAccumulate(t *testing.T) {
	org := "acme"
	cases := map[string]func(db *gorm.DB){
		"Model root":                        closureSitesMerged(nil, false, false),
		"Where root":                        closureSitesMerged(&org, false, false),
		"Where root, Session on Model only": closureSitesMerged(&org, true, false),
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, run)
			if len(got) != 2 {
				t.Fatalf("Expected 2 queries, got %d: %v", len(got), got)
			}
			if n := strings.Count(got[1], "id = ?"); n != 2 {
				t.Errorf("the second iteration should repeat the first one's condition, got %d: %s", n, got[1])
			}
		})
	}
}

// TestClosureSitesMergedRootsSessionIsolates asserts that a Session on both
// roots, the fix since #306, isolates every iteration.
func TestClosureSitesMergedRootsSessionIsolates(t *testing.T) {
	org := "acme"
	for name, o := range map[string]*string{"Model root": nil, "Where root": &org} {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, closureSitesMerged(o, true, true))
			subqueryWantSQL(t, got, []string{"id = ? AND id = ?"}, "id = ?", "id = ?")
		})
	}
}
