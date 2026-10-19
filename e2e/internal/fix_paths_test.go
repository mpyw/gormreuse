package internal

import (
	"testing"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixPathsPreserveQueries runs the fixtures of #202 as
// TestFixPreservesQueries does: reassigned values without a Session, a root
// that needs a Session only on the path past an if, and a root merged after
// its uses. The fixed code must keep every condition and leak none.
func TestFixPathsPreserveQueries(t *testing.T) {
	for _, tc := range fixPathsCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// fixPathsCases are the cases of TestFixPathsPreserveQueries.
var fixPathsCases = []fixsqlCase{
	{"Reassign", before.Reassign, after.Reassign},
	{"Combined", before.Combined, after.Combined},
	{"IfWithoutElseTaken", before.IfWithoutElseTaken, after.IfWithoutElseTaken},
	{"IfWithoutElseSkipped", before.IfWithoutElseSkipped, after.IfWithoutElseSkipped},
	{"PhiAfterUsesTaken", before.PhiAfterUsesTaken, after.PhiAfterUsesTaken},
	{"PhiAfterUsesSkipped", before.PhiAfterUsesSkipped, after.PhiAfterUsesSkipped},
}
