package internal

import (
	"slices"
	"strings"
	"testing"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// inPlaceParamCases are the fixtures of #387: a parameter that a helper
// changes in place, and one whose caller passes a value that may be mutable.
// Neither gets a //gormreuse:immutable-param fix.
var inPlaceParamCases = []fixsqlCase{
	{"InPlaceList", before.InPlaceList, after.InPlaceList},
	// The case below gets a Session at the top of the body.
	{"InPlaceUnproved", before.InPlaceUnproved, after.InPlaceUnproved},
}

// The fixtures are in fixsql/before/in_place_param.go.
var _ = registerFixsqlCases(inPlaceParamCases)

// TestFixInPlaceParamPreservesQueries runs inPlaceParamCases as
// TestFixPreservesQueries does. Both queries of each fixed function must
// keep the tenant filter, and the second query of InPlaceUnproved must no
// longer have the first name.
func TestFixInPlaceParamPreservesQueries(t *testing.T) {
	has := func(query []string, cond string) bool {
		return slices.ContainsFunc(query, func(c string) bool { return strings.Contains(c, cond) })
	}
	for _, tc := range inPlaceParamCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
			if len(fixed) != 2 {
				t.Fatalf("want 2 queries, got %q", fixed)
			}
			for i, q := range fixed {
				if !has(q, "tenant_id = 7") {
					t.Errorf("fixed query %d lost the tenant filter: %q", i, q)
				}
			}
			if tc.name == "InPlaceUnproved" && has(fixed[1], `"x"`) {
				t.Errorf("the fixed second query still has the first name: %q", fixed[1])
			}
		})
	}
}
