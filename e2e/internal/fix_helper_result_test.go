package internal

import (
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixHelperResultPreservesQueries runs the fixtures of #312 as
// TestFixPreservesQueries does. A helper that changes its parameter in place
// and returns it gets its discarded statement reassigned only when every
// caller reads the result, which then carries the change back.
func TestFixHelperResultPreservesQueries(t *testing.T) {
	for _, tc := range helperResultCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// helperResultCases are the cases of TestFixHelperResultPreservesQueries.
var helperResultCases = []fixsqlCase{
	{"HelperResultDiscarded", before.HelperResultDiscarded, after.HelperResultDiscarded},
	{"HelperResultUsed", before.HelperResultUsed, after.HelperResultUsed},
}

// The fixtures are in fixsql/before/helper_result.go.
var _ = registerFixsqlCases(helperResultCases)

// TestFixHelperResultDetectsReassignment checks that the comparison in
// TestFixHelperResultPreservesQueries fails for the fix gormreuse suggested
// for HelperResultDiscarded before #312: a Session at the top of the helper
// and the reassignment of its discarded statement. The caller discards the
// result, so its query loses the tenant filter.
func TestFixHelperResultDetectsReassignment(t *testing.T) {
	tenant := func(tx *gorm.DB, out *[]before.User) *gorm.DB {
		tx = tx.Session(&gorm.Session{})
		tx.Where("age > ?", 20).Find(out)
		tx = tx.Where("tenant_id = ?", 7)
		return tx
	}
	wrong := func(db *gorm.DB) {
		q := db.Model(&before.User{}).Where("name = ?", "x")
		tenant(q, &[]before.User{})
		q.Find(&[]before.User{})
	}
	buggy := fixsqlQueries(t, before.HelperResultDiscarded)
	if problems := fixsqlCompare(buggy, fixsqlQueries(t, wrong)); len(problems) == 0 {
		t.Error("the reassignment in the helper passed the comparison")
	}
}
