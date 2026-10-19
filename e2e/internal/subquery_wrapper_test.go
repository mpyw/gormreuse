package internal

import (
	"database/sql"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A subquery wrapped in gorm.Expr, sql.Named, a map or a clause.Expr, or
// passed as a condition of FirstOrInit or FirstOrCreate, is only read too
// (#253). GORM binds the wrapped *gorm.DB with Statement.AddVar when the
// outer query builds its SQL, as for a bare subquery (see subquery_test.go).

// TestSubqueryWrapperReuseIsRead uses one mid-chain value in each wrapper
// twice, then runs it on its own. No use changes it.
func TestSubqueryWrapperReuseIsRead(t *testing.T) {
	const sub = "(SELECT `id` FROM `users` WHERE age > ?)"
	uses := map[string]func(db, sub *gorm.DB){
		"gorm.Expr": func(db, sub *gorm.DB) { db.Where("id IN (?)", gorm.Expr("?", sub)).Find(&[]User{}) },
		"sql.Named": func(db, sub *gorm.DB) { db.Where("id IN (@s)", sql.Named("s", sub)).Find(&[]User{}) },
		"map": func(db, sub *gorm.DB) {
			db.Where("id IN (@s)", map[string]any{"s": sub}).Find(&[]User{})
		},
		"clause.Expr": func(db, sub *gorm.DB) {
			db.Where(clause.Expr{SQL: "id IN (?)", Vars: []any{sub}}).Find(&[]User{})
		},
		"FirstOrInit": func(db, sub *gorm.DB) { db.FirstOrInit(&User{}, "id IN (?)", sub) },
		"FirstOrCreate": func(db, sub *gorm.DB) {
			db.FirstOrCreate(&User{}, "id IN (?)", sub)
		},
	}
	for name, use := range uses {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, func(db *gorm.DB) {
				s := db.Model(&User{}).Select("id").Where("age > ?", 18) // clone==0
				use(db, s)
				use(db, s)
				s.Find(&[]User{})
			})
			subqueryWantSQL(t, got, []string{"age > ? AND"}, sub, sub, "SELECT `id` FROM `users` WHERE age > ?")
		})
	}
}

// TestSubqueryWrapperLaterChangeLeaks asserts that a wrapped subquery is read
// when the outer query runs, so a change before that leaks into it, as for a
// bare subquery.
func TestSubqueryWrapperLaterChangeLeaks(t *testing.T) {
	got := subqueryOuterSQL(t, func(db *gorm.DB) {
		sub := db.Model(&User{}).Select("id").Where("age > ?", 18) // clone==0
		x := db.Where("id IN (?)", gorm.Expr("?", sub))
		sub.Where("name = ?", "z").Find(&[]User{})
		x.Find(&[]User{})
	})
	if len(got) != 2 {
		t.Fatalf("Expected 2 queries, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[1], "(SELECT `id` FROM `users` WHERE age > ? AND name = ?)") {
		t.Errorf("the change before the outer query should leak into the subquery: %s", got[1])
	}
}
