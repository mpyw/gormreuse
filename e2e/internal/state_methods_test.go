package internal

import (
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// The runtime premises of testdata/src/gormreuse/state_methods.go (#307).
// AddError, DB, Get, InstanceGet and Callback touch no clause of a clone==0
// receiver, so a later query of it runs the same SQL whether or not they ran.
// AddError sets db.Error, which a later chain of the same value inherits.
// ToSQL builds its SQL from a copy of the receiver's Statement, so it reads
// the receiver without changing it. Migrator keeps a copy that shares the
// receiver's Statement, so it is not one of them.

// stateMethodsBaseline is the SQL of the later query when nothing ran before.
const stateMethodsBaseline = "SELECT * FROM `users` WHERE x = ? AND a = ?"

// stateMethodsLaterSQL runs call on a clone==0 value, then a query of the
// same value, and returns the SQL of every query that ran after call.
func stateMethodsLaterSQL(t *testing.T, call func(q *gorm.DB)) []string {
	t.Helper()
	db, captured := setupDB(t)
	q := db.Model(&User{}).Where("x = ?", 1) // clone==0
	call(q)
	before := len(*captured)
	q.Where("a = ?", 1).Find(&[]User{})
	return (*captured)[before:]
}

// TestStateMethods_TouchNoClause: the later query is the same as with no call
// before it.
func TestStateMethods_TouchNoClause(t *testing.T) {
	calls := map[string]func(q *gorm.DB){
		"nothing":     func(*gorm.DB) {},
		"DB":          func(q *gorm.DB) { _, _ = q.DB() },
		"Get":         func(q *gorm.DB) { _, _ = q.Get("k") },
		"InstanceGet": func(q *gorm.DB) { _, _ = q.InstanceGet("k") },
		"Callback":    func(q *gorm.DB) { _ = q.Callback() },
		"AddError(nil)": func(q *gorm.DB) {
			if err := q.AddError(nil); err != nil {
				t.Errorf("AddError(nil) = %v, want nil", err)
			}
		},
		// A real error, cleared again: it leaves the clauses as they were.
		"AddError then clear": func(q *gorm.DB) {
			_ = q.AddError(errors.New("boom"))
			q.Error = nil
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			got := stateMethodsLaterSQL(t, call)
			if len(got) != 1 || got[0] != stateMethodsBaseline {
				t.Errorf("later query = %q, want [%q]", got, stateMethodsBaseline)
			}
		})
	}
}

// TestStateMethods_AddErrorIsInherited: the error that AddError sets is the
// value's own, so a later chain of it does not run and returns that error.
// This is what AddError is for, and the analyzer does not count it as a
// branch: it changes no clause.
func TestStateMethods_AddErrorIsInherited(t *testing.T) {
	boom := errors.New("boom")
	db, captured := setupDB(t)
	q := db.Model(&User{}).Where("x = ?", 1)
	_ = q.AddError(boom)
	if err := q.Where("a = ?", 1).Find(&[]User{}).Error; !errors.Is(err, boom) {
		t.Errorf("later query error = %v, want %v", err, boom)
	}
	// The query callbacks run, but they build and run no SQL.
	for _, sql := range *captured {
		if sql != "" {
			t.Errorf("later query ran: %q", sql)
		}
	}
}

// TestStateMethods_SettingsSurviveBranch: Get reads Statement.Settings, which
// a chain of where conditions does not change.
func TestStateMethods_SettingsSurviveBranch(t *testing.T) {
	db, _ := setupDB(t)
	q := db.Model(&User{}).Where("x = ?", 1).Set("k", 1).InstanceSet("k", 2)
	q.Where("a = ?", 1).Find(&[]User{})
	if v, ok := q.Get("k"); !ok || v != 1 {
		t.Errorf("Get(k) = %v, %v, want 1, true", v, ok)
	}
	if v, ok := q.InstanceGet("k"); !ok || v != 2 {
		t.Errorf("InstanceGet(k) = %v, %v, want 2, true", v, ok)
	}
}

// TestStateMethods_ToSQLReadsReceiver: ToSQL sees a condition that a branch
// added to the receiver before it, so a branch before it is reported.
func TestStateMethods_ToSQLReadsReceiver(t *testing.T) {
	db, _ := setupDB(t)
	q := db.Model(&User{}).Where("x = ?", 1)
	q.Where("b = ?", 2).Find(&[]User{})
	sql := q.ToSQL(func(tx *gorm.DB) *gorm.DB { return tx.Find(&[]User{}) })
	if !strings.Contains(sql, "b = 2") {
		t.Errorf("ToSQL should read the branch's condition: %s", sql)
	}
}

// TestStateMethods_ToSQLDoesNotChangeReceiver: what ToSQL's callback adds
// stays in its copy, so ToSQL is not a branch.
func TestStateMethods_ToSQLDoesNotChangeReceiver(t *testing.T) {
	got := stateMethodsLaterSQL(t, func(q *gorm.DB) {
		_ = q.ToSQL(func(tx *gorm.DB) *gorm.DB { return tx.Where("c = ?", 3).Find(&[]User{}) })
	})
	if len(got) != 1 || got[0] != stateMethodsBaseline {
		t.Errorf("later query = %q, want [%q]", got, stateMethodsBaseline)
	}
}

// TestStateMethods_MigratorSharesStatement: the Migrator keeps a Session copy
// of a clone==0 receiver, so a condition added to the receiver after the
// Migrator call leaks into a query the Migrator runs. The analyzer cannot
// follow the copy through the Migrator interface, so Migrator (and
// AutoMigrate) stay branches.
func TestStateMethods_MigratorSharesStatement(t *testing.T) {
	db, _ := setupDB(t)
	var rowSQL []string
	if err := db.Callback().Row().After("gorm:row").Register("capture_row", func(tx *gorm.DB) {
		rowSQL = append(rowSQL, tx.Statement.SQL.String())
	}); err != nil {
		t.Fatalf("Failed to register capture callback: %v", err)
	}
	q := db.Model(&User{}).Where("x = ?", 1)
	m := q.Migrator()
	q.Where("late = ?", 2)
	_, _ = m.ColumnTypes(&User{})
	for _, sql := range rowSQL {
		if strings.Contains(sql, "late = ?") {
			return
		}
	}
	t.Errorf("a later condition should leak into the Migrator's queries: %q", rowSQL)
}
