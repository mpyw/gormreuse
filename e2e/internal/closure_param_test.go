package internal

import (
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// closureParamCases are the fixtures of #313: a closure's reused parameter
// gets a Session at the top of the closure's body.
var closureParamCases = []fixsqlCase{
	{"ClosureParam", before.ClosureParam, after.ClosureParam},
	{"ClosureParamIIFE", before.ClosureParamIIFE, after.ClosureParamIIFE},
}

// closureParamPaginateCases is Paginate in a closure, which
// TestFixPreservesQueries cannot compare (see paginateCases).
var closureParamPaginateCases = []fixsqlCase{
	{"ClosureParamPaginate", before.ClosureParamPaginate, after.ClosureParamPaginate},
}

// The fixtures are in fixsql/before/closure_param.go.
var _ = registerFixsqlCases(closureParamCases, closureParamPaginateCases)

// TestClosureParamPreservesQueries runs closureParamCases as
// TestFixPreservesQueries does, and checks that the second query of the fixed
// code no longer has the name of the first.
func TestClosureParamPreservesQueries(t *testing.T) {
	for _, tc := range closureParamCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
			if len(buggy) != 2 || len(buggy[1]) != 3 {
				t.Fatalf("the buggy second query should have the tenant and both names: %q", buggy)
			}
			if len(fixed[1]) != 2 {
				t.Errorf("the fixed second query should have the tenant and its own name: %q", fixed[1])
			}
		})
	}
}

// TestClosureParamPaginateStopsLeaks checks that the fixed closure leaks
// nothing from Count into Find, and that the buggy one does.
func TestClosureParamPaginateStopsLeaks(t *testing.T) {
	for _, tc := range closureParamPaginateCases {
		t.Run(tc.name, func(t *testing.T) {
			if problem := laterLoopListsAfterCount(laterLoopQueries(t, tc.after)); problem != "" {
				t.Errorf("fixed: %s", problem)
			}
			if laterLoopListsAfterCount(laterLoopQueries(t, tc.before)) == "" {
				t.Error("the buggy code passed the check")
			}
		})
	}
}

// TestClosureParamSessionKeepsTransaction checks that the body Session of a
// Transaction wrapper's callback still runs its queries in the transaction:
// the Session keeps the Statement's connection, which is the *sql.Tx.
func TestClosureParamSessionKeepsTransaction(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, true)
	withTx := func(db *gorm.DB, fn func(tx *gorm.DB) error) error { return db.Transaction(fn) }
	err := withTx(db, func(tx *gorm.DB) error {
		pool := tx.Statement.ConnPool
		tx = tx.Session(&gorm.Session{})
		if tx.Statement.ConnPool != pool {
			t.Error("the Session left the transaction's connection")
		}
		tx.Model(&before.User{}).Where("name = ?", "x").Find(&[]before.User{})
		tx.Model(&before.User{}).Where("name = ?", "y").Find(&[]before.User{})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if len(*captured) != 2 {
		t.Fatalf("want 2 queries, got %q", *captured)
	}
}
