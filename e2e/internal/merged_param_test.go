package internal

import (
	"slices"
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixMergedParamPreservesQueries runs the fixtures of #373 as
// TestFixPreservesQueries does: values merged through a Phi, on a parameter,
// whose fix must keep every condition, and on a local root, whose fix must
// keep them in one pass.
func TestFixMergedParamPreservesQueries(t *testing.T) {
	for _, tc := range mergedParamCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// mergedParamCases are the cases of TestFixMergedParamPreservesQueries.
var mergedParamCases = []fixsqlCase{
	{"MergedParamLoop", before.MergedParamLoop, after.MergedParamLoop},
	{"MergedParamScope", before.MergedParamScope, after.MergedParamScope},
	// The cases below get a fix.
	{"MergedLoopAfterIf", before.MergedLoopAfterIf, after.MergedLoopAfterIf},
	{"MergedLaterLoop", before.MergedLaterLoop, after.MergedLaterLoop},
	{"MergedInnerLoop", before.MergedInnerLoop, after.MergedInnerLoop},
}

// The fixtures are in fixsql/before/merged_param.go.
var _ = registerFixsqlCases(mergedParamCases)

// TestFixMergedParamDetectsOldFixes checks that the comparison fails for the
// fixes gormreuse suggested before #373. On the parameter, the body Session
// and the Where("a")'s Session made the discarded Where("b") change a copy.
// The reassignment of the tenant filter kept it inside the function, so the
// caller's query lost it.
func TestFixMergedParamDetectsOldFixes(t *testing.T) {
	loop := func(q *gorm.DB, c bool, items []int) {
		q = q.Session(&gorm.Session{})
		for range items {
			if c {
				q = q.Where("a = ?", 1).Session(&gorm.Session{})
			} else {
				q.Find(&[]before.User{})
			}
			q.Where("b = ?", 2)
		}
		q.Find(&[]before.User{})
	}
	scope := func(q *gorm.DB, admin bool, tenants []int) {
		if admin {
			q = q.Where("admin = ?", true)
		}
		q.Find(&[]before.User{})
		for _, t := range tenants {
			q = q.Where("tenant_id = ?", t).Session(&gorm.Session{})
		}
	}
	cases := []struct {
		name          string
		before, wrong func(*gorm.DB)
	}{
		{"MergedParamLoop", before.MergedParamLoop, func(db *gorm.DB) {
			loop(db.Model(&before.User{}), true, []int{1, 2})
		}},
		{"MergedParamScope", before.MergedParamScope, func(db *gorm.DB) {
			base := db.Model(&before.User{})
			scope(base, false, []int{7, 8})
			base.Find(&[]before.User{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			wrong := fixsqlQueries(t, tc.wrong)
			if problems := fixsqlCompare(buggy, wrong); len(problems) == 0 {
				t.Errorf("the fix suggested before passed the comparison\nbuggy: %q\nwrong: %q", buggy, wrong)
			}
		})
	}
}

// TestFixMergedLaterLoopOnePass checks that one pass of the fix stops the
// leak of MergedLaterLoop. The first pass suggested before gave the Where("b")
// on the loop's edge no Session, so each query of the last loop added its
// condition to the next one.
func TestFixMergedLaterLoopOnePass(t *testing.T) {
	firstPass := func(db *gorm.DB) {
		q := db.Model(&before.User{}).Session(&gorm.Session{})
		q.Find(&[]before.User{})
		for range 2 {
			q = q.Where("b = ?", 2)
		}
		for i := range 2 {
			q.Where("i = ?", i).Find(&[]before.User{})
		}
	}
	leaks := func(queries [][]string) bool {
		return slices.Contains(queries[len(queries)-1], "i = 0")
	}
	if !leaks(fixsqlQueries(t, firstPass)) {
		t.Fatal("the first pass suggested before does not leak; the test no longer shows the bug")
	}
	if fixed := fixsqlQueries(t, after.MergedLaterLoop); leaks(fixed) {
		t.Errorf("the fixed code still leaks: %q", fixed)
	}
}
