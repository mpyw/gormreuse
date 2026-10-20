package internal

import (
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixGoDeferOnlyPreservesQueries runs the fixtures of #309 as
// TestFixPreservesQueries does. A root used only in defer statements gets a
// Session at the root.
func TestFixGoDeferOnlyPreservesQueries(t *testing.T) {
	for _, tc := range goDeferOnlyCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// goDeferOnlyCases are the cases of TestFixGoDeferOnlyPreservesQueries.
var goDeferOnlyCases = []fixsqlCase{
	{"DeferOnly", before.DeferOnly, after.DeferOnly},
}

// The fixtures are in fixsql/before/go_defer_only.go.
var _ = registerFixsqlCases(goDeferOnlyCases)

// TestFixGoDeferOnlyDetectsDeferredChange checks why a deferred chain gets no
// fix when another deferred call uses the root (#309). The deferred Where
// runs first and adds its condition to q in place, so the deferred Find reads
// it. With a Session at the root, Where changes a copy and Find loses the
// condition, which the comparison catches.
func TestFixGoDeferOnlyDetectsDeferredChange(t *testing.T) {
	buggy := fixsqlQueries(t, func(db *gorm.DB) {
		q := db.Model(&before.User{}).Where("tenant_id = ?", 7)
		defer q.Find(&[]before.User{})
		defer q.Where("age > ?", 20)
	})
	wrong := fixsqlQueries(t, func(db *gorm.DB) {
		q := db.Model(&before.User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
		defer q.Find(&[]before.User{})
		defer q.Where("age > ?", 20)
	})
	if problems := fixsqlCompare(buggy, wrong); len(problems) == 0 {
		t.Errorf("a Session at the root passed the comparison\nbuggy: %q\nwrong: %q", buggy, wrong)
	}
}
