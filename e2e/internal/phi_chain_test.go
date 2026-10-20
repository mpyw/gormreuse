package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// phiChainCases are the fixtures of #343: pagination with several
// conditional filters. TestFixPreservesQueries cannot compare them, for the
// reason given at paginateCases.
var phiChainCases = []fixsqlCase{
	{"PhiChain", before.PhiChain, after.PhiChain},
	{"PhiChainParam", before.PhiChainParam, after.PhiChainParam},
	{"PhiChainLoop", before.PhiChainLoop, after.PhiChainLoop},
	{"PhiChainParamLoop", before.PhiChainParamLoop, after.PhiChainParamLoop},
}

// phiChainHelperCases is the fixture of #343 whose fix must keep a helper's
// in-place change.
var phiChainHelperCases = []fixsqlCase{
	{"PhiChainHelper", before.PhiChainHelper, after.PhiChainHelper},
}

// The fixtures are in fixsql/before/phi_chain.go.
var _ = registerFixsqlCases(phiChainCases, phiChainHelperCases)

// TestPhiChainStopsLeaks checks that one pass of fixes leaves the list query
// of each phiChainCases function with every filter and without Count's
// count(*), and that the first pass before #343, which the "pass1"
// functions copy, does not: Find then failed to scan.
func TestPhiChainStopsLeaks(t *testing.T) {
	pass1 := map[string]func(*gorm.DB){
		"PhiChain": func(db *gorm.DB) {
			var total int64
			q := db.Model(&before.User{}).Session(&gorm.Session{})
			q = q.Where("name = ?", "x").Session(&gorm.Session{})
			q = q.Where("email = ?", "x@example.com")
			q = q.Where("role = ?", "admin")
			q.Count(&total)
			q.Offset(10).Limit(10).Find(&[]before.User{})
		},
		"PhiChainParam": func(db *gorm.DB) {
			var total int64
			db = db.Model(&before.User{}).Session(&gorm.Session{})
			db = db.Where("name = ?", "x").Session(&gorm.Session{})
			db = db.Where("email = ?", "x@example.com")
			db = db.Where("role = ?", "admin")
			db.Count(&total)
			db.Offset(10).Limit(10).Find(&[]before.User{})
		},
		"PhiChainLoop": func(db *gorm.DB) {
			var total int64
			q := db.Model(&before.User{}).Where("name = ?", "x")
			for _, id := range []int{1, 2} {
				q = q.Where("id <> ?", id).Session(&gorm.Session{})
			}
			q = q.Where("role = ?", "admin")
			q.Count(&total)
			q.Offset(10).Limit(10).Find(&[]before.User{})
		},
		"PhiChainParamLoop": func(db *gorm.DB) {
			var total int64
			db = db.Model(&before.User{}).Where("name = ?", "x")
			for _, id := range []int{1, 2} {
				db = db.Where("id <> ?", id).Session(&gorm.Session{})
			}
			db = db.Where("role = ?", "admin")
			db.Count(&total)
			db.Offset(10).Limit(10).Find(&[]before.User{})
		},
	}
	for _, tc := range phiChainCases {
		t.Run(tc.name, func(t *testing.T) {
			want := []string{"name = 'x'", "role = 'admin'", "LIMIT 10 OFFSET 10"}
			if strings.Contains(tc.name, "Loop") {
				want = append(want, "id <> 1", "id <> 2")
			} else {
				want = append(want, "email = 'x@example.com'")
			}
			if problem := phiChainListed(laterLoopQueries(t, tc.after), want); problem != "" {
				t.Errorf("fixed: %s", problem)
			}
			if phiChainListed(laterLoopQueries(t, pass1[tc.name]), want) == "" {
				t.Error("the fix of the first pass before #343 passed the check")
			}
		})
	}
}

// phiChainListed returns a problem unless the second of the two queries has
// each of want, has no count(*), and ran without error.
func phiChainListed(queries []laterLoopQuery, want []string) string {
	if len(queries) != 2 {
		return "want 2 queries, got " + laterLoopSQL(queries)
	}
	list := queries[1]
	for _, w := range want {
		if !strings.Contains(list.sql, w) {
			return "the list query lacks " + w + ": " + laterLoopSQL(queries)
		}
	}
	if strings.Contains(list.sql, "count(") || list.err != nil {
		return "the list query has Count's leftovers: " + laterLoopSQL(queries)
	}
	return ""
}

// TestPhiChainHelperKeepsTenant checks that the fix of PhiChainHelper keeps
// the tenant that the helper adds in place, in both queries, and that a
// Session on the root, which following the Phis back past the helper would
// add, loses it.
func TestPhiChainHelperKeepsTenant(t *testing.T) {
	tenant := func(queries []laterLoopQuery) string {
		if len(queries) != 2 {
			return "want 2 queries, got " + laterLoopSQL(queries)
		}
		for _, q := range queries {
			if !strings.Contains(q.sql, "tenant_id = 7") {
				return "a query lost the tenant: " + laterLoopSQL(queries)
			}
		}
		return ""
	}
	if problem := tenant(laterLoopQueries(t, after.PhiChainHelper)); problem != "" {
		t.Errorf("fixed: %s", problem)
	}
	wrong := func(db *gorm.DB) {
		var total int64
		q := db.Model(&before.User{}).Session(&gorm.Session{})
		q = q.Where("name = ?", "x").Session(&gorm.Session{})
		q.Where("tenant_id = ?", 7)
		for _, id := range []int{1, 2} {
			q = q.Where("id <> ?", id).Session(&gorm.Session{})
		}
		q = q.Where("role = ?", "admin").Session(&gorm.Session{})
		q.Count(&total)
		q.Offset(10).Limit(10).Find(&[]before.User{})
	}
	if tenant(laterLoopQueries(t, wrong)) == "" {
		t.Error("a Session on the root kept the tenant")
	}
}
