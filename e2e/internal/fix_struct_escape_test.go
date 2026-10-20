package internal

import (
	"testing"

	"gorm.io/gorm"

	"github.com/mpyw/gormreuse/e2e/internal/fixsql/after"
	"github.com/mpyw/gormreuse/e2e/internal/fixsql/before"
)

// TestFixStructEscapePreservesQueries runs the fixtures of #310 as
// TestFixPreservesQueries does. Each stores q in a struct that reaches code
// which changes q in place: a method of the struct, or a function that takes
// it by value, by pointer or nested in another struct. A Session at q makes
// that code change a copy, so the fixed code must keep the tenant filter.
func TestFixStructEscapePreservesQueries(t *testing.T) {
	for _, tc := range fixStructEscapeCases {
		t.Run(tc.name, func(t *testing.T) {
			buggy := fixsqlQueries(t, tc.before)
			fixed := fixsqlQueries(t, tc.after)
			for _, problem := range fixsqlCompare(buggy, fixed) {
				t.Error(problem)
			}
		})
	}
}

// fixStructEscapeCases are the cases of TestFixStructEscapePreservesQueries.
var fixStructEscapeCases = []fixsqlCase{
	{"StructReceiver", before.StructReceiver, after.StructReceiver},
	{"StructLiteral", before.StructLiteral, after.StructLiteral},
	{"StructPointerLiteral", before.StructPointerLiteral, after.StructPointerLiteral},
	{"StructFieldStore", before.StructFieldStore, after.StructFieldStore},
	{"StructNested", before.StructNested, after.StructNested},
	{"StructParam", before.StructParam, after.StructParam},
	// The case below still gets a fix.
	{"StructAfterLastUse", before.StructAfterLastUse, after.StructAfterLastUse},
}

// structEscapeSvc and structEscapeParams mirror the structs of fixsql/before
// for the fixes gormreuse suggested before #310.
type (
	structEscapeSvc    struct{ db *gorm.DB }
	structEscapeParams struct{ db *gorm.DB }
	structEscapeOuter  struct{ in structEscapeParams }
)

func (s *structEscapeSvc) apply() { s.db.Where("tenant_id = ?", 7) }

// TestFixStructEscapeDetectsSessionAtRoot checks that the comparison in
// TestFixStructEscapePreservesQueries fails for the fixes gormreuse suggested
// before #310: a Session at the root, or at the top of the body for a
// parameter root. The tenant filter is then added to a copy.
func TestFixStructEscapeDetectsSessionAtRoot(t *testing.T) {
	queries := func(q *gorm.DB) {
		q.Where("name = ?", "x").Find(&[]before.User{})
		q.Where("name = ?", "y").Find(&[]before.User{})
	}
	root := func(db *gorm.DB) *gorm.DB { return db.Model(&before.User{}).Session(&gorm.Session{}) }
	applyParams := func(p structEscapeParams) { p.db.Where("tenant_id = ?", 7) }
	applyParamsPtr := func(p *structEscapeParams) { p.db.Where("tenant_id = ?", 7) }
	applyOuter := func(o structEscapeOuter) { o.in.db.Where("tenant_id = ?", 7) }
	cases := []struct {
		name          string
		before, wrong func(*gorm.DB)
	}{
		{"StructReceiver", before.StructReceiver, func(db *gorm.DB) {
			q := root(db)
			s := &structEscapeSvc{db: q}
			s.apply()
			queries(q)
		}},
		{"StructLiteral", before.StructLiteral, func(db *gorm.DB) {
			q := root(db)
			applyParams(structEscapeParams{db: q})
			queries(q)
		}},
		{"StructPointerLiteral", before.StructPointerLiteral, func(db *gorm.DB) {
			q := root(db)
			applyParamsPtr(&structEscapeParams{db: q})
			queries(q)
		}},
		{"StructFieldStore", before.StructFieldStore, func(db *gorm.DB) {
			q := root(db)
			var p structEscapeParams
			p.db = q
			applyParamsPtr(&p)
			queries(q)
		}},
		{"StructNested", before.StructNested, func(db *gorm.DB) {
			q := root(db)
			applyOuter(structEscapeOuter{in: structEscapeParams{db: q}})
			queries(q)
		}},
		{"StructParam", before.StructParam, func(db *gorm.DB) {
			tx := db.Model(&before.User{})
			tx = tx.Session(&gorm.Session{})
			s := &structEscapeSvc{db: tx}
			s.apply()
			queries(tx)
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
