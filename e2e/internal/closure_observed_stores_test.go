package internal

import (
	"context"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// These tests pin the runtime premise of #303: a closure that runs after its
// creation, here queued and run later or used as a range-over-func body, sees
// the value its captured variable holds when it runs. After db =
// db.WithContext(ctx) on every path, that is the WithContext result, so even a
// mid-chain argument does not accumulate across the closures.

// laterGroup runs the queued functions in order at Wait, as errgroup would run
// them later. The queries run one at a time, so the captured SQL is ordered.
type laterGroup struct{ fs []func() error }

func (g *laterGroup) Go(f func() error) { g.fs = append(g.fs, f) }

func (g *laterGroup) Wait() error {
	for _, f := range g.fs {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// observedRebindGroup is the errgroup shape of #303.
func observedRebindGroup(ctx context.Context, db *gorm.DB) error {
	db = db.WithContext(ctx)
	var g laterGroup
	g.Go(func() error { return db.Where("a = ?", 1).Find(&[]User{}).Error })
	g.Go(func() error { return db.Where("b = ?", 2).Find(&[]User{}).Error })
	return g.Wait()
}

// observedRebindRange is the range-over-func shape of #303.
func observedRebindRange(ctx context.Context, db *gorm.DB, ids []int) {
	db = db.WithContext(ctx)
	for _, id := range slices.All(ids) {
		db.Where("id = ?", id).Find(&[]User{})
	}
}

type observedDeps struct{ db *gorm.DB }

// observedRebindField is the field shape of #303: a captured pointer.
func observedRebindField(ctx context.Context, db *gorm.DB) {
	d := &observedDeps{db: db}
	d.db = d.db.WithContext(ctx)
	f := func(c string, v int) { d.db.Where(c, v).Find(&[]User{}) }
	f("a = ?", 1)
	f("b = ?", 2)
}

// observedQueries checks that each query keeps the caller's condition and
// carries exactly one of the closures' conditions.
func observedQueries(t *testing.T, captured []string, conds ...string) {
	t.Helper()
	if len(captured) < len(conds) {
		t.Fatalf("Expected at least %d queries, got %d", len(conds), len(captured))
	}
	for i, query := range captured[:len(conds)] {
		if !strings.Contains(query, "tenant =") {
			t.Errorf("query %d should keep the caller's condition: %s", i, query)
		}
		for j, cond := range conds {
			if has := strings.Contains(query, cond); has != (i == j) {
				t.Errorf("query %d: contains %q = %v, want %v: %s", i, cond, has, i == j, query)
			}
		}
	}
}

// TestObservedRebindGroupIsolates: the closures queued after the rebind do
// not see each other's conditions.
func TestObservedRebindGroupIsolates(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, false)

	if err := observedRebindGroup(context.Background(), db.Model(&User{}).Where("tenant = ?", 9)); err != nil {
		t.Fatal(err)
	}
	observedQueries(t, *captured, "a =", "b =")
}

// TestObservedRebindRangeIsolates: likewise for a range-over-func body.
func TestObservedRebindRangeIsolates(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, false)

	observedRebindRange(context.Background(), db.Model(&User{}).Where("tenant = ?", 9), []int{1, 2})
	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	for i, query := range (*captured)[:2] {
		if !strings.Contains(query, "tenant =") || strings.Count(query, "id =") != 1 {
			t.Errorf("query %d should have the caller's condition and one id: %s", i, query)
		}
	}
}

// TestObservedRebindFieldIsolates: likewise for a field read through a
// captured pointer.
func TestObservedRebindFieldIsolates(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, false)

	observedRebindField(context.Background(), db.Model(&User{}).Where("tenant = ?", 9))
	observedQueries(t, *captured, "a =", "b =")
}

// TestObservedStoreAfterClosureAccumulates: a store after the closure is
// created is what the closure sees when it runs later, so a mutable value
// stored there and also used directly accumulates. The analyzer reports this
// shape.
func TestObservedStoreAfterClosureAccumulates(t *testing.T) {
	db, captured, _ := setupDBWithMock(t, false)

	q := db.Model(&User{}).Where("y = ?", 0)
	m := db.Model(&User{}).Where("x = ?", 1)
	var g laterGroup
	g.Go(func() error { return q.Where("a = ?", 2).Find(&[]User{}).Error })
	q = m
	m.Where("b = ?", 3).Find(&[]User{})
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}

	if len(*captured) < 2 {
		t.Fatalf("Expected at least 2 queries, got %d", len(*captured))
	}
	if second := (*captured)[1]; !strings.Contains(second, "x =") || !strings.Contains(second, "b =") {
		t.Errorf("the closure should read m and ACCUMULATE 'b': %s", second)
	}
}
