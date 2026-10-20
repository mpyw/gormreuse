package internal

import (
	"slices"
	"testing"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixRangeFuncStorePreservesQueries runs the fixtures of #341 as
// TestFixPreservesQueries does: a Session after the Where that a
// range-over-func body stores keeps each query after the loop to its own
// conditions.
func TestFixRangeFuncStorePreservesQueries(t *testing.T) {
	for _, tc := range rangeFuncStoreCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
			// The buggy second query reads the first one's condition, so a
			// fix that leaves it alone fixed nothing.
			if len(fixed) == 2 && slices.Equal(buggy[1], fixed[1]) {
				t.Errorf("the second query is unchanged: %q", fixed[1])
			}
		})
	}
}

// rangeFuncStoreCases are the cases of TestFixRangeFuncStorePreservesQueries.
var rangeFuncStoreCases = []fixsqlCase{
	{"RangeFuncFilter", before.RangeFuncFilter, after.RangeFuncFilter},
}

// The fixtures are in fixsql/before/range_func_store.go.
var _ = registerFixsqlCases(rangeFuncStoreCases)
