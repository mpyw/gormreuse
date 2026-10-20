package internal

import (
	"slices"
	"testing"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixHandoffPreservesQueries runs the fixtures of #347 as
// TestFixPreservesQueries does: a root handed to a helper that only runs
// queries on it, or to a recursive call of its own function, keeps every
// condition of each query once fixed, and loses the ones that leaked into it.
func TestFixHandoffPreservesQueries(t *testing.T) {
	for _, tc := range handoffCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
			// The buggy second query reads the first one's condition, so a
			// fix that leaves it alone fixed nothing.
			if len(fixed) > 1 && slices.Equal(buggy[1], fixed[1]) {
				t.Errorf("the second query is unchanged: %q", fixed[1])
			}
		})
	}
}

// handoffCases are the cases of TestFixHandoffPreservesQueries.
var handoffCases = []fixsqlCase{
	{"FinisherOnlyHelper", before.FinisherOnlyHelper, after.FinisherOnlyHelper},
	{"RecursiveHelper", before.RecursiveHelper, after.RecursiveHelper},
}

// The fixtures are in fixsql/before/handoff.go.
var _ = registerFixsqlCases(handoffCases)
