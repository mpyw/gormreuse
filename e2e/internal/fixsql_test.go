package internal

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixPreservesQueries runs each function of fixsql/before (the reuse bug)
// and fixsql/after (the same code with gormreuse's suggested fixes applied)
// against real GORM, and compares the SQL that each finisher emits (#199). A
// fix may stop conditions from leaking into later queries, but must never drop
// one the code applied:
//
//   - The first query of the fixed code has every condition of the first query
//     of the buggy code. Nothing has leaked into that one yet.
//   - Every condition the buggy code applied to any query is in some query of
//     the fixed code. This catches a discarded statement whose effect a fix
//     made disappear, such as q.Where("age > ?", x) after the loop's query.
//   - No fixed query has a condition its buggy counterpart lacks.
func TestFixPreservesQueries(t *testing.T) {
	for _, tc := range fixsqlCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// fixsqlCase is a function of fixsql/before and its fixed copy in
// fixsql/after.
type fixsqlCase struct {
	name          string
	before, after func(*gorm.DB)
}

// fixsqlCases are the cases of TestFixPreservesQueries.
// TestFixsqlCasesCoverBefore checks that they, fixPathsCases and
// fixEscapeCases cover every exported function of fixsql/before.
var fixsqlCases = []fixsqlCase{
	{"MultiCall", before.MultiCall, after.MultiCall},
	{"SingleCall", before.SingleCall, after.SingleCall},
	{"SessionOnly", before.SessionOnly, after.SessionOnly},
	{"Loop", before.Loop, after.Loop},
	{"LoopOnly", before.LoopOnly, after.LoopOnly},
	{"SessionLast", before.SessionLast, after.SessionLast},
	{"Closure", before.Closure, after.Closure},
	{"MethodValue", before.MethodValue, after.MethodValue},
	{"MethodValueDiscarded", before.MethodValueDiscarded, after.MethodValueDiscarded},
}

// TestFixsqlCasesCoverBefore checks that every exported function of
// fixsql/before is a case of TestFixPreservesQueries or
// TestFixPathsPreserveQueries, once, and that each case runs the functions it
// is named for. The case lists are written by hand, so a new fixture would
// otherwise run in no test (#264).
func TestFixsqlCasesCoverBefore(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("fixsql", "before", "before.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse fixsql/before: %v", err)
	}
	var want []string
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.IsExported() {
			want = append(want, fn.Name.Name)
		}
	}

	var got []string
	for _, tc := range slices.Concat(fixsqlCases, fixPathsCases, fixEscapeCases, laterLoopCases, paginateCases) {
		for pkg, fn := range map[string]func(*gorm.DB){"before": tc.before, "after": tc.after} {
			full := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
			if name := full[strings.LastIndex(full, "/")+1:]; name != pkg+"."+tc.name {
				t.Errorf("case %s runs %s, want %s.%s", tc.name, name, pkg, tc.name)
			}
		}
		got = append(got, tc.name)
	}

	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the cases are %q, want each exported function of fixsql/before once: %q", got, want)
	}
}

// TestFixPreservesQueriesDetectsSessionOnly checks that the comparison in
// TestFixPreservesQueries fails for the fixes gormreuse suggested before #199:
// a Session at the root and no reassignment of the discarded statement.
func TestFixPreservesQueriesDetectsSessionOnly(t *testing.T) {
	cases := []struct {
		name          string
		before, wrong func(*gorm.DB)
	}{
		{"MultiCall", before.MultiCall, func(db *gorm.DB) {
			q := db.Model(&before.User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
			q.Where("deleted_at IS NULL").Limit(10)
			q.Where("name = ?", "x").Find(&[]before.User{})
			q.Where("name = ?", "y").Find(&[]before.User{})
		}},
		{"Loop", before.Loop, func(db *gorm.DB) {
			q := db.Model(&before.User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
			for _, x := range []int{1, 2} {
				q.Where("id > ?", x).Find(&[]before.User{})
				q.Where("age > ?", x)
			}
		}},
		{"Closure", before.Closure, func(db *gorm.DB) {
			q := db.Model(&before.User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
			f := func() {
				q.Where("age > ?", 20)
			}
			q.Where("name = ?", "x").Find(&[]before.User{})
			f()
			q.Where("name = ?", "y").Find(&[]before.User{})
		}},
		{"MethodValueDiscarded", before.MethodValueDiscarded, func(db *gorm.DB) {
			q := db.Model(&before.User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
			where := q.Where
			where("age > ?", 20)
			where("name = ?", "x").Find(&[]before.User{})
			where("name = ?", "y").Find(&[]before.User{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			wrong := fixsqlQueries(t, tc.wrong)
			if problems := fixsqlCompare(buggy, wrong); len(problems) == 0 {
				t.Errorf("a Session-only fix passed the comparison\nbuggy: %q\nwrong: %q", buggy, wrong)
			}
		})
	}
}

// fixsqlQueries runs fn on a fresh mocked DB and returns the conditions of
// each query it emits, with the bound values filled in.
func fixsqlQueries(t *testing.T, fn func(*gorm.DB)) [][]string {
	t.Helper()
	db, _ := setupDB(t)
	var queries [][]string
	if err := db.Callback().Query().After("gorm:query").Register("fixsql", func(tx *gorm.DB) {
		sql := tx.Explain(tx.Statement.SQL.String(), tx.Statement.Vars...)
		queries = append(queries, fixsqlConditions(sql))
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	fn(db)
	if len(queries) == 0 {
		t.Fatal("no query ran")
	}
	return queries
}

// fixsqlTail matches the clauses after WHERE.
var fixsqlTail = regexp.MustCompile(` (ORDER BY|LIMIT|OFFSET) `)

// fixsqlConditions splits a SELECT into its WHERE conditions and the clauses
// after them, such as "LIMIT 10".
func fixsqlConditions(sql string) []string {
	_, where, ok := strings.Cut(sql, " WHERE ")
	if !ok {
		return nil
	}
	var tail []string
	if loc := fixsqlTail.FindStringIndex(where); loc != nil {
		rest := where[loc[0]+1:]
		where = where[:loc[0]]
		for {
			next := fixsqlTail.FindStringIndex(rest)
			if next == nil {
				tail = append(tail, rest)
				break
			}
			tail = append(tail, rest[:next[0]])
			rest = rest[next[0]+1:]
		}
	}
	return append(strings.Split(where, " AND "), tail...)
}

// fixsqlCompare returns a description of each way in which fixed, the
// conditions of each query of the fixed code, drops or adds a condition
// against buggy, those of the buggy code. See TestFixPreservesQueries.
func fixsqlCompare(buggy, fixed [][]string) []string {
	if len(buggy) != len(fixed) {
		return []string{fmt.Sprintf("the buggy code ran %d queries and the fixed code %d", len(buggy), len(fixed))}
	}
	var problems []string
	for _, c := range buggy[0] {
		if !slices.Contains(fixed[0], c) {
			problems = append(problems, fmt.Sprintf("the first fixed query lost %q: %q, want all of %q", c, fixed[0], buggy[0]))
		}
	}
	all := slices.Concat(fixed...)
	for i, q := range buggy {
		for _, c := range q {
			if !slices.Contains(all, c) {
				problems = append(problems, fmt.Sprintf("no fixed query has %q, which buggy query %d has", c, i))
			}
		}
	}
	for i := range fixed {
		for _, c := range fixed[i] {
			if !slices.Contains(buggy[i], c) {
				problems = append(problems, fmt.Sprintf("fixed query %d has %q, which buggy query %d lacks: %q", i, c, i, buggy[i]))
			}
		}
	}
	return problems
}
