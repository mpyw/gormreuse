package internal

import (
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixEscapePreservesQueries runs the fixtures of #243 as
// TestFixPreservesQueries does. Most change q in place through code other
// than a discarded statement on q: a helper, a closure, a container, or a
// chain whose Error is read. A Session at q makes that code change a copy,
// so the fixed code must keep every condition, such as the tenant filter,
// and every query that depends on q.Error.
func TestFixEscapePreservesQueries(t *testing.T) {
	for _, tc := range fixEscapeCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// fixEscapeCases are the cases of TestFixEscapePreservesQueries.
var fixEscapeCases = []fixsqlCase{
	{"Helper", before.Helper, after.Helper},
	{"HelperDiscard", before.HelperDiscard, after.HelperDiscard},
	{"ClosureArg", before.ClosureArg, after.ClosureArg},
	{"Slice", before.Slice, after.Slice},
	{"Map", before.Map, after.Map},
	{"Chan", before.Chan, after.Chan},
	{"ErrorRead", before.ErrorRead, after.ErrorRead},
	{"ResultDiscarded", before.ResultDiscarded, after.ResultDiscarded},
	{"ErrorPropagation", before.ErrorPropagation, after.ErrorPropagation},
	// The cases below still get a fix.
	{"EscapeLast", before.EscapeLast, after.EscapeLast},
	{"ErrorOfFinisher", before.ErrorOfFinisher, after.ErrorOfFinisher},
	{"ParamResult", before.ParamResult, after.ParamResult},
}

// TestFixEscapeDetectsSessionAtRoot checks that the comparison in
// TestFixEscapePreservesQueries fails for the fixes gormreuse suggested
// before #243: a Session at the root, and, for Helper, a helper whose
// discarded statements are reassigned to its parameter. The reassignment
// alone changes nothing while the caller passes a mutable value, but it makes
// the helper lint-clean, so nothing reports the change it then loses.
func TestFixEscapeDetectsSessionAtRoot(t *testing.T) {
	applyTenant := func(tx *gorm.DB, tenant int) {
		tx = tx.Where("tenant_id = ?", tenant)
		_ = tx.Where("deleted_at IS NULL")
	}
	queries := func(q *gorm.DB) {
		q.Where("name = ?", "x").Find(&[]before.User{})
		q.Where("name = ?", "y").Find(&[]before.User{})
	}
	root := func(db *gorm.DB) *gorm.DB { return db.Model(&before.User{}).Session(&gorm.Session{}) }
	cases := []struct {
		name          string
		before, wrong func(*gorm.DB)
	}{
		{"Helper", before.Helper, func(db *gorm.DB) {
			q := root(db)
			applyTenant(q, 7)
			queries(q)
		}},
		{"ClosureArg", before.ClosureArg, func(db *gorm.DB) {
			q := root(db)
			apply := func(d *gorm.DB) { d.Where("tenant_id = ?", 7) }
			apply(q)
			queries(q)
		}},
		{"Slice", before.Slice, func(db *gorm.DB) {
			q := root(db)
			qs := []*gorm.DB{q}
			qs[0].Where("tenant_id = ?", 7)
			queries(q)
		}},
		{"ErrorRead", before.ErrorRead, func(db *gorm.DB) {
			q := root(db)
			if err := q.Where("tenant_id = ?", 7).Error; err != nil {
				return
			}
			queries(q)
		}},
		{"ErrorPropagation", before.ErrorPropagation, func(db *gorm.DB) {
			q := root(db)
			q.Find(&[]before.User{})
			q.Where("age > ?", 20).Table("").Find(new(int))
			if q.Error != nil {
				db.Model(&before.User{}).Where("failed = ?", true).Find(&[]before.User{})
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			wrong := fixsqlQueries(t, tc.wrong)
			if problems := fixsqlCompare(buggy, wrong); len(problems) == 0 {
				t.Errorf("a Session-at-root fix passed the comparison\nbuggy: %q\nwrong: %q", buggy, wrong)
			}
		})
	}
}
