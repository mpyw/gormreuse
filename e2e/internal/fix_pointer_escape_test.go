package internal

import (
	"sort"
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixPointerEscapePreservesQueries runs the fixtures of #345 as
// TestFixPreservesQueries does. Each lets other code change q in place: a
// helper given the address of q, or code that a call runs while q is stored
// in a package-level struct. A Session at q makes that code change a copy, so
// the fixed code must keep the tenant filter.
func TestFixPointerEscapePreservesQueries(t *testing.T) {
	for _, tc := range pointerEscapeCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// pointerEscapeCases are the cases of TestFixPointerEscapePreservesQueries.
var pointerEscapeCases = []fixsqlCase{
	{"PointerAddr", before.PointerAddr, after.PointerAddr},
	{"PointerSlice", before.PointerSlice, after.PointerSlice},
	{"PointerConnection", before.PointerConnection, after.PointerConnection},
	{"PointerScopes", before.PointerScopes, after.PointerScopes},
	{"PointerSort", before.PointerSort, after.PointerSort},
	// The case below still gets a fix.
	{"PointerPrint", before.PointerPrint, after.PointerPrint},
}

// The fixtures are in fixsql/before/pointer_escape.go.
var _ = registerFixsqlCases(pointerEscapeCases)

// pointerEscapeSvc and pointerEscapeShared mirror the struct of fixsql/before
// for the fixes gormreuse suggested before #345.
type pointerEscapeSvc struct{ db *gorm.DB }

func (s *pointerEscapeSvc) apply() { s.db.Where("tenant_id = ?", 7) }

var pointerEscapeShared pointerEscapeSvc

// pointerEscapeSorter changes pointerEscapeShared.db in place from Len.
type pointerEscapeSorter []int

func (s pointerEscapeSorter) Len() int           { pointerEscapeShared.apply(); return len(s) }
func (s pointerEscapeSorter) Less(i, j int) bool { return s[i] < s[j] }
func (s pointerEscapeSorter) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// TestFixPointerEscapeDetectsSessionAtRoot checks that the comparison in
// TestFixPointerEscapePreservesQueries fails for the fixes gormreuse suggested
// before #345: a Session at q. The tenant filter is then added to a copy.
func TestFixPointerEscapeDetectsSessionAtRoot(t *testing.T) {
	queries := func(q *gorm.DB) {
		q.Where("name = ?", "x").Find(&[]before.User{})
		q.Where("name = ?", "y").Find(&[]before.User{})
	}
	root := func(db *gorm.DB) *gorm.DB { return db.Model(&before.User{}).Session(&gorm.Session{}) }
	apply := func(p **gorm.DB) { (*p).Where("tenant_id = ?", 7) }
	applyAll := func(ps []**gorm.DB) {
		for _, p := range ps {
			(*p).Where("tenant_id = ?", 7)
		}
	}
	cases := []struct {
		name          string
		before, wrong func(*gorm.DB)
	}{
		{"PointerAddr", before.PointerAddr, func(db *gorm.DB) {
			q := root(db)
			apply(&q)
			queries(q)
		}},
		{"PointerSlice", before.PointerSlice, func(db *gorm.DB) {
			q := root(db)
			applyAll([]**gorm.DB{&q})
			queries(q)
		}},
		{"PointerConnection", before.PointerConnection, func(db *gorm.DB) {
			q := root(db)
			pointerEscapeShared.db = q
			_ = db.Session(&gorm.Session{}).Connection(func(*gorm.DB) error { pointerEscapeShared.apply(); return nil })
			queries(q)
		}},
		{"PointerScopes", before.PointerScopes, func(db *gorm.DB) {
			q := root(db)
			pointerEscapeShared.db = q
			db.Session(&gorm.Session{}).Scopes(func(tx *gorm.DB) *gorm.DB { pointerEscapeShared.apply(); return tx }).Find(&[]before.User{})
			queries(q)
		}},
		{"PointerSort", before.PointerSort, func(db *gorm.DB) {
			q := root(db)
			pointerEscapeShared.db = q
			sort.Sort(pointerEscapeSorter{2, 1})
			queries(q)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			if problems := fixsqlCompare(buggy, fixsqlQueries(t, tc.wrong)); len(problems) == 0 {
				t.Error("the Session at the root passed the comparison")
			}
		})
	}
}
