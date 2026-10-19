package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// A *gorm.DB passed as an argument of a query-building method (a subquery, a
// group condition, a Joins condition) is only read (#185). GORM builds the
// subquery from v.Session(&Session{DryRun: true}).getInstance(), a cloned
// Statement, so the argument is never changed. The read happens when the
// outer query runs, so a change to the argument before that leaks into it.

type subqueryCompany struct {
	ID   uint
	Name string
}

type subqueryEmployee struct {
	ID        uint
	Name      string
	CompanyID uint
	Company   subqueryCompany
}

// subqueryOuterSQL runs fn and returns the SQL of every query it ran, without
// the dry-run queries GORM runs to build the subqueries.
func subqueryOuterSQL(t *testing.T, fn func(db *gorm.DB)) []string {
	t.Helper()
	db, _ := setupDB(t)
	var outer []string
	if err := db.Callback().Query().After("gorm:query").Register("capture_outer", func(tx *gorm.DB) {
		if !tx.DryRun {
			outer = append(outer, tx.Statement.SQL.String())
		}
	}); err != nil {
		t.Fatalf("Failed to register capture callback: %v", err)
	}
	fn(db)
	return outer
}

// subqueryWantSQL fails the test unless got has one entry per want and each
// entry contains its want and none of the forbidden strings.
func subqueryWantSQL(t *testing.T, got []string, forbidden []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Expected %d queries, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("query %d should contain %q: %s", i, w, got[i])
		}
		for _, f := range forbidden {
			if strings.Contains(got[i], f) {
				t.Errorf("query %d should not contain %q: %s", i, f, got[i])
			}
		}
	}
}

// TestSubqueryReuseIsRead uses one mid-chain value as an argument of each
// read-only method twice, then runs it on its own. No use changes it.
func TestSubqueryReuseIsRead(t *testing.T) {
	uses := map[string]struct {
		use  func(db, sub *gorm.DB)
		want string
	}{
		"Where": {
			func(db, sub *gorm.DB) { db.Where("id IN (?)", sub).Find(&[]User{}) },
			"WHERE id IN (SELECT `id` FROM `users` WHERE age > ?)",
		},
		"Or": {
			func(db, sub *gorm.DB) { db.Where("name = ?", "x").Or("id IN (?)", sub).Find(&[]User{}) },
			"OR id IN (SELECT `id` FROM `users` WHERE age > ?)",
		},
		"Not": {
			func(db, sub *gorm.DB) { db.Not("id IN (?)", sub).Find(&[]User{}) },
			"NOT id IN (SELECT `id` FROM `users` WHERE age > ?)",
		},
		"Having": {
			func(db, sub *gorm.DB) {
				db.Model(&User{}).Select("age").Group("age").Having("age IN (?)", sub).Find(&[]User{})
			},
			"HAVING age IN (SELECT `id` FROM `users` WHERE age > ?)",
		},
		"Table": {
			func(db, sub *gorm.DB) { db.Table("(?) as x", sub).Find(&[]User{}) },
			"FROM (SELECT `id` FROM `users` WHERE age > ?) as x",
		},
		"Select": {
			func(db, sub *gorm.DB) { db.Model(&User{}).Select("(?) AS n", sub).Find(&[]User{}) },
			"SELECT (SELECT `id` FROM `users` WHERE age > ?) AS n",
		},
		"Joins": {
			func(db, sub *gorm.DB) { db.Model(&User{}).Joins("JOIN (?) s ON s.id = users.id", sub).Find(&[]User{}) },
			"JOIN (SELECT `id` FROM `users` WHERE age > ?) s",
		},
		"Raw": {
			func(db, sub *gorm.DB) { db.Raw("SELECT * FROM users WHERE id IN (?)", sub).Find(&[]User{}) },
			"WHERE id IN (SELECT `id` FROM `users` WHERE age > ?)",
		},
		"Find conds": {
			func(db, sub *gorm.DB) { db.Find(&[]User{}, "id IN (?)", sub) },
			"WHERE id IN (SELECT `id` FROM `users` WHERE age > ?)",
		},
		"group condition": {
			func(db, sub *gorm.DB) { db.Where(sub).Find(&[]User{}) },
			"WHERE age > ?",
		},
	}
	for name, tc := range uses {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, func(db *gorm.DB) {
				sub := db.Model(&User{}).Select("id").Where("age > ?", 18) // clone==0
				tc.use(db, sub)
				tc.use(db, sub)
				sub.Find(&[]User{})
			})
			subqueryWantSQL(t, got, []string{"age > ? AND"}, tc.want, tc.want, "SELECT `id` FROM `users` WHERE age > ?")
		})
	}
}

// TestSubqueryJoinsConditionReuseIsRead covers Joins with a *gorm.DB holding
// the join conditions of an association.
func TestSubqueryJoinsConditionReuseIsRead(t *testing.T) {
	got := subqueryOuterSQL(t, func(db *gorm.DB) {
		j := db.Where(&subqueryCompany{Name: "acme"}) // clone==0
		db.Joins("Company", j).Find(&[]subqueryEmployee{})
		db.Joins("Company", j).Find(&[]subqueryEmployee{})
		j.Find(&[]subqueryCompany{})
	})
	join := "`Company`.`name` = ?"
	subqueryWantSQL(t, got, []string{"`name` = ? AND"}, join, join, "WHERE `subquery_companies`.`name` = ?")
}

// TestSubqueryLaterChangeLeaks is the caveat of #185: the subquery is built
// when the outer query runs, so a change to the argument between the call
// that takes it and the outer query shows up in the outer query.
func TestSubqueryLaterChangeLeaks(t *testing.T) {
	changes := map[string]func(sub *gorm.DB){
		"branch": func(sub *gorm.DB) { sub.Where("name = ?", "z").Find(&[]User{}) },
		"reassignment": func(sub *gorm.DB) {
			sub = sub.Where("name = ?", "z")
			sub.Find(&[]User{})
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			got := subqueryOuterSQL(t, func(db *gorm.DB) {
				sub := db.Model(&User{}).Select("id").Where("age > ?", 18) // clone==0
				x := db.Where("id IN (?)", sub)
				change(sub)
				x.Find(&[]User{})
			})
			if len(got) != 2 {
				t.Fatalf("Expected 2 queries, got %d: %v", len(got), got)
			}
			if !strings.Contains(got[1], "IN (SELECT `id` FROM `users` WHERE age > ? AND name = ?)") {
				t.Errorf("the change before the outer query should leak into the subquery: %s", got[1])
			}
		})
	}
}

// TestSubqueryEarlierBranchLeaks is the other order: a branch of the argument
// before the call changes its Statement in place, so the subquery sees it.
func TestSubqueryEarlierBranchLeaks(t *testing.T) {
	got := subqueryOuterSQL(t, func(db *gorm.DB) {
		sub := db.Model(&User{}).Select("id").Where("age > ?", 18) // clone==0
		sub.Where("name = ?", "z").Find(&[]User{})
		db.Where("id IN (?)", sub).Find(&[]User{})
	})
	if len(got) != 2 {
		t.Fatalf("Expected 2 queries, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[1], "IN (SELECT `id` FROM `users` WHERE age > ? AND name = ?)") {
		t.Errorf("an earlier branch of the argument should leak into the subquery: %s", got[1])
	}
}

// TestSubqueryChangeAfterOuterQueryIsSafe asserts that a change after the
// outer query ran does not reach that query.
func TestSubqueryChangeAfterOuterQueryIsSafe(t *testing.T) {
	got := subqueryOuterSQL(t, func(db *gorm.DB) {
		sub := db.Model(&User{}).Select("id").Where("age > ?", 18) // clone==0
		db.Where("id IN (?)", sub).Find(&[]User{})
		sub.Where("name = ?", "z").Find(&[]User{})
	})
	subqueryWantSQL(t, got, nil, "IN (SELECT `id` FROM `users` WHERE age > ?)", "name = ?")
	if len(got) == 2 && strings.Contains(got[0], "name") {
		t.Errorf("the outer query ran before the change: %s", got[0])
	}
}
