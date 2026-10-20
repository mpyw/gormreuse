package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// laterLoopCases are the fixtures of #311 with a reassignment read in a
// later loop. One pass of fixes must leave them correct at runtime.
var laterLoopCases = []fixsqlCase{
	{"LaterLoop", before.LaterLoop, after.LaterLoop},
	{"LaterLoopInIf", before.LaterLoopInIf, after.LaterLoopInIf},
}

// paginateCases are the fixtures of #311 with a parameter merged with a
// Where, as in pagination. TestLaterLoopPreservesQueries cannot compare them:
// in the buggy code, Count's error stays on db, so Find never runs. Count
// fails on the mocked rows, which have several columns, and so does Find
// when Count's Select leaks into it. TestLaterLoopMergeStopsLeaks checks
// them.
var paginateCases = []fixsqlCase{
	{"Paginate", before.Paginate, after.Paginate},
	{"PaginateIsolated", before.PaginateIsolated, after.PaginateIsolated},
}

// TestLaterLoopPreservesQueries runs laterLoopCases as
// TestFixPreservesQueries does.
func TestLaterLoopPreservesQueries(t *testing.T) {
	for _, tc := range laterLoopCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// TestLaterLoopMergeStopsLeaks checks that the fixed code of #311 leaks
// nothing from one query into the next. TestLaterLoopPreservesQueries
// allows a fixed query to keep what its buggy counterpart leaked, so it
// passes for the fixes of the first -fix pass before #311 too, which the
// "pass1" functions copy. The fixed code must pass here, and those must not.
func TestLaterLoopMergeStopsLeaks(t *testing.T) {
	cases := []struct {
		name         string
		fixed, pass1 func(*gorm.DB)
		check        func([]laterLoopQuery) string
	}{
		{"LaterLoop", after.LaterLoop, func(db *gorm.DB) {
			q := db.Model(&before.User{}).Where("tenant_id = ?", 7)
			q = q.Where("age > ?", 20)
			for _, id := range []int{1, 2, 3} {
				q.Where("id = ?", id).Find(&[]before.User{})
			}
		}, laterLoopOneIDEach},
		{"LaterLoopInIf", after.LaterLoopInIf, func(db *gorm.DB) {
			q := db.Model(&before.User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
			if true {
				q = q.Where("age > ?", 20)
			}
			for _, id := range []int{1, 2, 3} {
				q.Where("id = ?", id).Find(&[]before.User{})
			}
		}, laterLoopOneIDEach},
		{"Paginate", after.Paginate, func(db *gorm.DB) {
			db = db.Model(&before.User{}).Session(&gorm.Session{})
			var total int64
			if name := "x"; name != "" {
				db = db.Where("name = ?", name)
			}
			db.Count(&total)
			db.Offset(10).Limit(10).Find(&[]before.User{})
		}, laterLoopListsAfterCount},
		{"PaginateIsolated", after.PaginateIsolated, func(db *gorm.DB) {
			db = db.Model(&before.User{}).Session(&gorm.Session{})
			var total int64
			if name := "x"; name != "" {
				db = db.Where("name = ?", name)
			}
			db.Count(&total)
			db.Offset(10).Limit(10).Find(&[]before.User{})
		}, laterLoopListsAfterCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if problem := tc.check(laterLoopQueries(t, tc.fixed)); problem != "" {
				t.Errorf("fixed: %s", problem)
			}
			if tc.check(laterLoopQueries(t, tc.pass1)) == "" {
				t.Error("the fix of the first pass before #311 passed the check")
			}
		})
	}
}

// laterLoopQuery is the SQL of a query and the error it ended with.
type laterLoopQuery struct {
	sql string
	err error
}

// laterLoopQueries runs fn on a fresh mocked DB and returns each query it
// emits, with the bound values filled in.
func laterLoopQueries(t *testing.T, fn func(*gorm.DB)) []laterLoopQuery {
	t.Helper()
	db, _ := setupDB(t)
	var queries []laterLoopQuery
	if err := db.Callback().Query().After("gorm:query").Register("laterloop", func(tx *gorm.DB) {
		queries = append(queries, laterLoopQuery{tx.Explain(tx.Statement.SQL.String(), tx.Statement.Vars...), tx.Error})
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	fn(db)
	return queries
}

// laterLoopOneIDEach returns a problem unless each of the three queries has
// exactly one id condition.
func laterLoopOneIDEach(queries []laterLoopQuery) string {
	if len(queries) != 3 {
		return "want 3 queries, got " + laterLoopSQL(queries)
	}
	for _, q := range queries {
		if strings.Count(q.sql, " id = ") != 1 {
			return "a query has an id condition of an earlier one: " + laterLoopSQL(queries)
		}
	}
	return ""
}

// laterLoopListsAfterCount returns a problem unless the second of the two
// queries lists the page of users that match the name, without Count's
// count(*), and without error.
func laterLoopListsAfterCount(queries []laterLoopQuery) string {
	if len(queries) != 2 {
		return "want 2 queries, got " + laterLoopSQL(queries)
	}
	list := queries[1]
	if strings.Contains(list.sql, "count(") || !strings.Contains(list.sql, "name = 'x' LIMIT 10 OFFSET 10") || list.err != nil {
		return "the list query has Count's leftovers: " + laterLoopSQL(queries)
	}
	return ""
}

// laterLoopSQL joins the SQL and errors of queries for a message.
func laterLoopSQL(queries []laterLoopQuery) string {
	var parts []string
	for _, q := range queries {
		parts = append(parts, q.sql)
		if q.err != nil {
			parts = append(parts, "error: "+q.err.Error())
		}
	}
	return strings.Join(parts, " | ")
}
