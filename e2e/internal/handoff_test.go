package internal

import (
	"context"
	"maps"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// handoffDeps is a struct that carries a *gorm.DB to a helper (#341).
type handoffDeps struct{ DB *gorm.DB }

// handoffUseDeps branches the handle it gets through the struct.
func handoffUseDeps(d handoffDeps) {
	d.DB.Where("age > ?", 1).Find(&[]User{})
}

// handoffRepo is a receiver that holds a *gorm.DB (#341).
type handoffRepo struct{ db *gorm.DB }

// active branches the handle the receiver holds.
func (r *handoffRepo) active() {
	r.db.Where("active = ?", true).Find(&[]User{})
}

// handoffSecondQuery runs two queries and returns the SQL of the second one.
func handoffSecondQuery(t *testing.T, run func(db *gorm.DB)) string {
	t.Helper()
	db, captured := setupDB(t)
	run(db)
	if len(*captured) != 2 {
		t.Fatalf("want 2 queries, got %q", *captured)
	}
	return (*captured)[1]
}

// TestHandoffStructArgLeak shows the reuse that #341 reports for a struct
// handed to a function: the helper's condition reaches the later query on q.
func TestHandoffStructArgLeak(t *testing.T) {
	find := handoffSecondQuery(t, func(db *gorm.DB) {
		q := db.Where("name = ?", "x")
		handoffUseDeps(handoffDeps{DB: q})
		q.Find(&[]User{})
	})
	if !strings.Contains(find, "age >") {
		t.Errorf("the later query did not read the helper's condition: %s", find)
	}
}

// TestHandoffStructArgIsolated checks that a Session at q's root keeps the
// helper's condition out of the later query.
func TestHandoffStructArgIsolated(t *testing.T) {
	find := handoffSecondQuery(t, func(db *gorm.DB) {
		q := db.Where("name = ?", "x").Session(&gorm.Session{})
		handoffUseDeps(handoffDeps{DB: q})
		q.Find(&[]User{})
	})
	if strings.Contains(find, "age >") || !strings.Contains(find, "name =") {
		t.Errorf("the later query read the helper's condition or lost its own: %s", find)
	}
}

// TestHandoffReceiverLeak shows the same reuse through the receiver of a
// method: r.active() changes the Statement that q.Find then reads.
func TestHandoffReceiverLeak(t *testing.T) {
	find := handoffSecondQuery(t, func(db *gorm.DB) {
		q := db.Where("name = ?", "x")
		r := &handoffRepo{db: q}
		r.active()
		q.Find(&[]User{})
	})
	if !strings.Contains(find, "active") {
		t.Errorf("the later query did not read the method's condition: %s", find)
	}
}

// handoffFiltered builds q in a range-over-func loop, as #341 reports, and
// runs two queries on it. isolate ends each Where with a Session, which is
// the suggested fix.
func handoffFiltered(db *gorm.DB, isolate bool) {
	q := db.WithContext(context.Background())
	for k, v := range maps.All(map[string]any{"name": "x"}) {
		q = q.Where(k+" = ?", v)
		if isolate {
			q = q.Session(&gorm.Session{})
		}
	}
	q.Where("age > ?", 1).Find(&[]User{})
	q.Find(&[]User{})
}

// TestHandoffRangeFuncLeak shows that the Where stored in a range-over-func
// body is the value after the loop: the first query's condition reaches the
// second one. The analyzer saw only the WithContext before #341.
func TestHandoffRangeFuncLeak(t *testing.T) {
	find := handoffSecondQuery(t, func(db *gorm.DB) { handoffFiltered(db, false) })
	if !strings.Contains(find, "age >") {
		t.Errorf("the second query did not read the first one's condition: %s", find)
	}
}

// TestHandoffRangeFuncIsolated checks the suggested fix.
func TestHandoffRangeFuncIsolated(t *testing.T) {
	find := handoffSecondQuery(t, func(db *gorm.DB) { handoffFiltered(db, true) })
	if strings.Contains(find, "age >") || !strings.Contains(find, "name =") {
		t.Errorf("the second query read the first one's condition or lost the filter: %s", find)
	}
}
