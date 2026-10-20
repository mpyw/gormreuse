package internal

import (
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixErrorPropagationPreservesQueries runs the fixtures of #372 as
// TestFixPreservesQueries does. A query on a mutable root that fails leaves
// its error in the root's Error, so gorm skips the queries after it, and a
// later read of Error returns it. The fixed code must keep the query that
// runs only when that error is returned.
func TestFixErrorPropagationPreservesQueries(t *testing.T) {
	for _, tc := range errorPropagationCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// errorPropagationCases are the cases of
// TestFixErrorPropagationPreservesQueries.
var errorPropagationCases = []fixsqlCase{
	{"ErrorReturned", before.ErrorReturned, after.ErrorReturned},
	{"ErrorReturnedParam", before.ErrorReturnedParam, after.ErrorReturnedParam},
	// The case below still gets a fix.
	{"ErrorChecked", before.ErrorChecked, after.ErrorChecked},
}

// The fixtures are in fixsql/before/error_propagation.go.
var _ = registerFixsqlCases(errorPropagationCases)

// TestFixErrorPropagationDetectsSessionAtRoot checks that the comparison in
// TestFixErrorPropagationPreservesQueries fails for the fixes gormreuse
// suggested before #372: a Session at the root, and at the top of the body
// for a parameter. The second query then runs on its own copy and returns
// nil, so the query that reports the error never runs.
func TestFixErrorPropagationDetectsSessionAtRoot(t *testing.T) {
	report := func(db *gorm.DB, err error) {
		if err != nil {
			db.Model(&before.User{}).Where("failed = ?", true).Find(&[]before.User{})
		}
	}
	returned := func(q *gorm.DB) error {
		q.Find(new(int))
		return q.Where("name = ?", "x").Find(&[]before.User{}).Error
	}
	cases := []struct {
		name          string
		before, wrong func(*gorm.DB)
	}{
		{"ErrorReturned", before.ErrorReturned, func(db *gorm.DB) {
			report(db, returned(db.Model(&before.User{}).Session(&gorm.Session{})))
		}},
		{"ErrorReturnedParam", before.ErrorReturnedParam, func(db *gorm.DB) {
			report(db, func(q *gorm.DB) error {
				q = q.Session(&gorm.Session{})
				return returned(q)
			}(db.Model(&before.User{})))
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
