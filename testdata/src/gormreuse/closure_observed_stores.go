package internal

import (
	"context"
	"slices"

	"gorm.io/gorm"
)

// =============================================================================
// Stores a closure observes (#303)
//
// A closure that is deferred, spawned, passed on, stored or used as a
// range-over-func body runs at some point after its MakeClosure. A captured,
// reassigned variable read there sees the stores that reach the MakeClosure
// and every store that can run after it. The same holds for a field read
// through a captured pointer.
// =============================================================================

type cosUser struct{ ID int }

// cosGroup stands in for errgroup.Group.
type cosGroup struct{}

func (g *cosGroup) Go(f func() error) {}
func (g *cosGroup) Wait() error       { return nil }

type cosDeps struct{ db *gorm.DB }

// ===== SHOULD NOT REPORT =====

// cosErrgroup: every path rebinds db before the closures are created, so each
// spawned closure reads the WithContext result, not the parameter.
func cosErrgroup(ctx context.Context, db *gorm.DB, ids []int) error {
	db = db.WithContext(ctx)
	var g cosGroup
	for _, id := range ids {
		g.Go(func() error { return db.First(&cosUser{}, id).Error })
	}
	return g.Wait()
}

// cosErrgroupTwo: the same with two spawned closures.
func cosErrgroupTwo(ctx context.Context, db *gorm.DB) error {
	db = db.WithContext(ctx)
	var g cosGroup
	g.Go(func() error { return db.Where("a = 1").Find(nil).Error })
	g.Go(func() error { return db.Where("b = 2").Find(nil).Error })
	return g.Wait()
}

// cosRangeFunc: the body of a range-over-func loop is a closure the iterator
// calls.
func cosRangeFunc(ctx context.Context, db *gorm.DB, us []cosUser) {
	db = db.WithContext(ctx)
	for _, u := range slices.All(us) {
		db.Model(&u).Update("org", 1)
	}
}

// cosTransaction: a callback passed to Transaction.
func cosTransaction(ctx context.Context, db *gorm.DB) error {
	db = db.WithContext(ctx)
	return db.Transaction(func(tx *gorm.DB) error {
		db.Where("a = 1").Find(nil)
		return db.Where("b = 2").Find(nil).Error
	})
}

// cosSession: a Session taken the same way is not a second copy.
func cosSession(db *gorm.DB) error {
	db = db.Session(&gorm.Session{})
	var g cosGroup
	g.Go(func() error { return db.Where("a = 1").Find(nil).Error })
	g.Go(func() error { return db.Where("b = 2").Find(nil).Error })
	return g.Wait()
}

// cosDeferred: deferred closures created after the rebind.
func cosDeferred(ctx context.Context, db *gorm.DB) {
	db = db.WithContext(ctx)
	defer func() { db.Where("a = 1").Find(nil) }()
	defer func() { db.Where("b = 2").Find(nil) }()
}

// cosNested: a closure nested in a spawned one observes what the outer one
// does.
func cosNested(ctx context.Context, db *gorm.DB) {
	db = db.WithContext(ctx)
	go func() {
		func() { db.Where("a = 1").Find(nil) }()
		db.Where("b = 2").Find(nil)
	}()
}

// cosFieldNested: the same for a field read through a captured pointer.
func cosFieldNested(ctx context.Context, db *gorm.DB) {
	d := &cosDeps{db: db}
	d.db = d.db.WithContext(ctx)
	go func() {
		func() { d.db.Where("a = 1").Find(nil) }()
		d.db.Where("b = 2").Find(nil)
	}()
}

// cosMk returns a fresh struct holding a chain.
func cosMk(db *gorm.DB) *cosDeps { return &cosDeps{db: db.Where("x")} }

// cosReturnedInLoop: a closure in a loop reads the field of the struct this
// iteration's call returned. The walk after the closure stops at the call,
// which returns a new struct on the next iteration.
func cosReturnedInLoop(db *gorm.DB, ids []int) {
	s := db.Session(&gorm.Session{})
	var g cosGroup
	for range ids {
		p := cosMk(s)
		g.Go(func() error { return p.db.Find(nil).Error })
	}
}

// cosReturnedReassigned: [LIMITATION] the closure may read either struct, and
// the walk after it stops at the second call. The last read sees only the
// second struct, but a load of a reassigned pointer variable is resolved to
// every pointer stored in it, so it is matched to the first struct too.
func cosReturnedReassigned(db *gorm.DB) {
	s := db.Session(&gorm.Session{})
	p := cosMk(s)
	go func() { p.db.Find(nil) }()
	p = cosMk(s)
	p.db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// cosFieldCalled: a field read through a captured pointer, in a closure that
// is only called, sees the stores that reach the calls.
func cosFieldCalled(ctx context.Context, db *gorm.DB) {
	d := &cosDeps{db: db}
	d.db = d.db.WithContext(ctx)
	f := func(c string) { d.db.Where(c).Find(&[]cosUser{}) }
	f("a = 1")
	f("b = 2")
}

// cosFieldSpawned: the same in spawned closures.
func cosFieldSpawned(ctx context.Context, db *gorm.DB) error {
	d := &cosDeps{db: db}
	d.db = d.db.WithContext(ctx)
	var g cosGroup
	g.Go(func() error { return d.db.Where("a = 1").Find(nil).Error })
	g.Go(func() error { return d.db.Where("b = 2").Find(nil).Error })
	return g.Wait()
}

// cosFieldCapturedOuterRead: a pointer variable a closure captures lives in an
// Alloc, so d.db = ... stores through a load of it. The function's own reads
// see that store.
func cosFieldCapturedOuterRead(ctx context.Context, db *gorm.DB) {
	d := &cosDeps{db: db}
	d.db = d.db.WithContext(ctx)
	d.db.Where("a = 1").Find(nil)
	d.db.Where("b = 2").Find(nil)
	go func() { d.db.Where("c = 3").Find(nil) }()
}

// ===== SHOULD REPORT =====

// cosStoreAfterSpawn: the goroutine may run after q is rebound to m, which is
// also used here.
func cosStoreAfterSpawn(db, other *gorm.DB) {
	q := other.Where("y")
	m := db.Where("x")
	go func() { q.Find(nil) }()
	q = m
	m.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// cosStoreInLoop: a store later in the loop body runs before the next
// iteration's closure is created, and maybe before this one runs.
func cosStoreInLoop(db, other *gorm.DB, ids []int) {
	q := other.Where("y")
	m := db.Where("x")
	m.Find(nil)
	var g cosGroup
	for range ids {
		g.Go(func() error { return q.Find(nil).Error }) // want `\*gorm\.DB reused: second branch from mutable root`
		q = m
	}
}

// cosStoreBeforeSpawn: the stores that reach the closure still count.
func cosStoreBeforeSpawn(db *gorm.DB) {
	q := db.Where("x")
	q = q.Where("y")
	go func() { q.Find(nil) }()
	go func() { q.Count(nil) }() // want `\*gorm\.DB reused: second branch from mutable root`
}

// cosFieldStoreAfterSpawn: a field store after the closure is seen too.
func cosFieldStoreAfterSpawn(db, other *gorm.DB) {
	d := &cosDeps{db: other.Where("y")}
	m := db.Where("x")
	go func() { d.db.Find(nil) }()
	d.db = m
	m.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
