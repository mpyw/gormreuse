package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// The runtime premises of testdata/src/gormreuse/call_site_modes.go (#254,
// #252): a call through a method value, a go or defer statement, and a
// variable that may hold one of two roots branch the value as a direct call
// does.

// TestCallSiteBoundReassignIsSafe asserts that q = w("a"), with w := q.Where,
// reassigns q like q = q.Where("a"): the later use sees one chain, with no
// leftover from another branch.
func TestCallSiteBoundReassignIsSafe(t *testing.T) {
	got := subqueryOuterSQL(t, func(db *gorm.DB) {
		q := db.Where("x = ?", 1) // clone==0
		for _, c := range []string{"a", "b"} {
			w := q.Where
			q = w(c+" = ?", 2)
		}
		q.Find(&[]User{})
	})
	subqueryWantSQL(t, got, nil, "x = ? AND a = ? AND b = ?")
}

// TestCallSiteBoundDeferLeaks asserts that defer f(nil), with f := q.Find,
// runs after the function's other uses of q, so their changes leak into it.
func TestCallSiteBoundDeferLeaks(t *testing.T) {
	got := subqueryOuterSQL(t, func(db *gorm.DB) {
		q := db.Where("x = ?", 1) // clone==0
		f := q.Find
		defer f(&[]User{})
		q.Where("y = ?", 2).Find(&[]User{})
	})
	if len(got) != 2 {
		t.Fatalf("Expected 2 queries, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[1], "y = ?") {
		t.Errorf("the deferred call should see the change made before it runs: %s", got[1])
	}
}

// TestCallSitePhiSecondRootLeaks asserts that q.Find(nil), with q holding a
// on one path, branches a: a later use of a sees the change.
func TestCallSitePhiSecondRootLeaks(t *testing.T) {
	got := subqueryOuterSQL(t, func(db *gorm.DB) {
		a := db.Where("a = ?", 1) // clone==0
		b := db.Where("b = ?", 2)
		q := b
		if cond := t.Name() != ""; cond { // holds: q is a
			q = a
		}
		q.Where("y = ?", 3).Find(&[]User{})
		a.Find(&[]User{})
	})
	if len(got) != 2 {
		t.Fatalf("Expected 2 queries, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[1], "y = ?") {
		t.Errorf("the branch through q should leak into a: %s", got[1])
	}
}
