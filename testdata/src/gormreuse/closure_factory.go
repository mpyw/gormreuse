package internal

import (
	"context"

	"gorm.io/gorm"
)

// =============================================================================
// Closure factories and rebound captured variables (#251)
// =============================================================================

type cfUser struct{}

// ===== SHOULD NOT REPORT =====

// cfFactoryTwice: each call of base builds a new chain from an immutable db,
// so the two queries are independent (WHERE c / WHERE d at runtime).
func cfFactoryTwice(db *gorm.DB) {
	db = db.Session(&gorm.Session{})
	base := func() *gorm.DB { return db.Model(&cfUser{}) }
	base().Where("c").Find(nil)
	base().Where("d").Find(nil)
}

// cfFactoryLoop: one call site in a loop runs the closure once per iteration.
func cfFactoryLoop(db *gorm.DB, ids []int) {
	db = db.Session(&gorm.Session{})
	q := func(id int) *gorm.DB { return db.Where("id = ?", id) }
	for _, id := range ids {
		q(id).Find(nil)
	}
}

// cfFactoryDeclaredInLoop: the closure is created and called on each iteration.
func cfFactoryDeclaredInLoop(db *gorm.DB, ids []int) {
	s := db.Session(&gorm.Session{})
	for _, id := range ids {
		q := func() *gorm.DB { return s.Where("id = ?", id) }
		q().Find(nil)
	}
}

// cfFactoryCapturedSession: the captured variable is never reassigned.
func cfFactoryCapturedSession(db *gorm.DB) {
	s := db.Session(&gorm.Session{})
	base := func() *gorm.DB { return s.Model(&cfUser{}).Where("a") }
	base().Where("c").Find(nil)
	base().Where("d").Find(nil)
}

// cfFactoryBranches: every return builds a fresh chain.
func cfFactoryBranches(db *gorm.DB, c bool) {
	s := db.Session(&gorm.Session{})
	base := func() *gorm.DB {
		if c {
			return s.Where("a")
		}
		return s.Where("b")
	}
	base().Find(nil)
	base().Count(nil)
}

// cfFactoryLoopBuilt: a chain the closure extends in a loop is still built
// anew on each call.
func cfFactoryLoopBuilt(db *gorm.DB, a, b []string) {
	s := db.Session(&gorm.Session{})
	base := func(conds []string) *gorm.DB {
		q := s.Where("a")
		for _, c := range conds {
			q = q.Where(c)
		}
		return q.Where("z")
	}
	base(a).Find(nil)
	base(b).Find(nil)
}

// cfRebind: the captured parameter is rebound before the IIFE runs, so the
// closure reads the WithContext result, not the parameter.
func cfRebind(ctx context.Context, db *gorm.DB) {
	db = db.WithContext(ctx)
	db.Find(nil)
	func() { db.Count(nil) }()
}

// cfRebindParentReads: the reads in the enclosing function see the rebinding
// too, though a closure captures the variable.
func cfRebindParentReads(ctx context.Context, db *gorm.DB) {
	db = db.WithContext(ctx)
	db.Find(nil)
	db.Count(nil)
	func() { _ = db }()
}

// cfRebindCalledLater: f's only use is the call, which runs after the
// rebinding.
func cfRebindCalledLater(ctx context.Context, db *gorm.DB) {
	f := func() { db.Count(nil) }
	db = db.WithContext(ctx)
	db.Find(nil)
	f()
}

// cfRebindDeferred: a deferred closure may run at any point, so its read sees
// every store, the parameter included. That is still a single use of the
// parameter besides WithContext.
func cfRebindDeferred(ctx context.Context, db *gorm.DB) {
	db = db.WithContext(ctx)
	defer func() { db.Count(nil) }()
	db.Find(nil)
}

// cfRebindSpawned: likewise for a goroutine.
func cfRebindSpawned(ctx context.Context, db *gorm.DB) {
	db = db.WithContext(ctx)
	go func() { db.Count(nil) }()
	db.Find(nil)
}

// ===== SHOULD REPORT =====

// cfFactoryBodyUse: the chain the closure returns is also used inside it, so
// the caller's use is a second branch of it on each call.
func cfFactoryBodyUse(db *gorm.DB) {
	db = db.Session(&gorm.Session{})
	base := func() *gorm.DB {
		q := db.Where("x")
		q.Find(nil)
		return q
	}
	base().Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	base().Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// cfFactoryCapturedRoot: every call branches the same captured mutable q, so
// the body runs q.Where twice (#249) and the calls share its result.
func cfFactoryCapturedRoot(db *gorm.DB) {
	q := db.Where("x")
	f := func() *gorm.DB { return q.Where("y") } // want `\*gorm\.DB reused: second branch from mutable root`
	f().Find(nil)
	f().Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// cfFactoryParam: a closure parameter is not fresh on each call.
func cfFactoryParam(db *gorm.DB) {
	q := db.Where("x")
	f := func(d *gorm.DB) *gorm.DB { return d.Where("y") }
	f(q).Find(nil)
	f(q).Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// cfFactoryHelper: the result of another function may be shared by every call,
// and each call passes the parameter db again (#249).
func cfFactoryHelper(db *gorm.DB) {
	f := func() *gorm.DB { return cfHelper(db) } // want `\*gorm\.DB reused: second branch from mutable root`
	f().Find(nil)
	f().Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func cfHelper(db *gorm.DB) *gorm.DB { return db.Where("x") }

// cfRebindToMutable: the closure runs after q is rebound, so it reads "b",
// which the Count after it reuses. The fix also gives "a" a Session, which is
// redundant: a store before two reads of a captured variable gets one.
func cfRebindToMutable(db *gorm.DB) {
	s := db.Session(&gorm.Session{})
	q := s.Where("a")
	f := func() { q.Find(nil) }
	q.Find(nil)
	q = s.Where("b")
	f()
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// cfRebindEscapes: a closure passed on may run at any point, so its read may
// see the first store.
func cfRebindEscapes(db *gorm.DB) {
	s := db.Session(&gorm.Session{})
	q := s.Where("a")
	f := func() { q.Find(nil) }
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q = s.Where("b")
	cfRun(f)
}

func cfRun(f func()) { f() }
