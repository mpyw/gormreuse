package internal

import (
	"context"

	"gorm.io/gorm"
)

// =============================================================================
// Calls through a captured local function variable (#304)
//
// A local variable that holds a function and that a closure captures is an
// Alloc in SSA, read through a FreeVar inside the closure. A call through it
// is the call of the function it holds when the variable is assigned exactly
// once, so it carries every directive of that function, in the declaring
// function and in the closure alike. Each case below has a twin without the
// capture, which behaves the same.
// =============================================================================

// cfvGroup stands in for errgroup.Group: Go runs f in a goroutine.
type cfvGroup struct{}

func (*cfvGroup) Go(f func() error) { go func() { _ = f() }() }

// --- pure ---

func cfvPure(db *gorm.DB) {
	//gormreuse:pure
	peek := func(q *gorm.DB) {}
	q := db.Where("x")
	peek(q)
	q.Find(nil) // OK: peek is pure
	func() { peek(nil) }()
}

func cfvPureInClosure(db *gorm.DB) {
	//gormreuse:pure
	peek := func(q *gorm.DB) {}
	func() {
		q := db.Where("x")
		peek(q)
		q.Find(nil) // OK: peek is pure, read through the closure's FreeVar
	}()
}

func cfvPureUncaptured(db *gorm.DB) {
	//gormreuse:pure
	peek := func(q *gorm.DB) {}
	q := db.Where("x")
	peek(q)
	q.Find(nil) // OK: peek is pure
}

// --- immutable-return, declared and inferred ---

func cfvImmutableReturn(db *gorm.DB) {
	//gormreuse:immutable-return
	conn := func() *gorm.DB { return db.Session(&gorm.Session{}) }
	q := conn()
	q.Find(nil)
	q.Count(nil) // OK: conn returns an immutable value
	func() { _ = conn() }()
}

func cfvInferredImmutableReturn(db *gorm.DB) {
	conn := func() *gorm.DB { return db.Session(&gorm.Session{}) }
	q := conn()
	q.Find(nil)
	q.Count(nil) // OK: conn's body returns a Session result
	func() { _ = conn() }()
}

func cfvInferredImmutableReturnInClosure(db *gorm.DB) {
	conn := func() *gorm.DB { return db.Session(&gorm.Session{}) }
	func() {
		q := conn()
		q.Find(nil)
		q.Count(nil) // OK: conn's body returns a Session result
	}()
}

// cfvMutableReturn: conn runs twice, so its body branches the parameter
// twice, as without the capture.
func cfvMutableReturn(db *gorm.DB) {
	conn := func() *gorm.DB { return db.Where("x") } // want `\*gorm\.DB reused: second branch from mutable root`
	q := conn()
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	func() { _ = conn() }()
}

// --- a closure factory called twice still builds a new chain on each call ---

func cfvFactory(db *gorm.DB) {
	s := db.Session(&gorm.Session{})
	base := func() *gorm.DB { return s.Model(nil) }
	base().Where("c").Find(nil)
	base().Where("d").Find(nil) // OK: a new Statement on each call
	func() { base().Find(nil) }()
}

// cfvFactoryReassigned: a stored closure reads every store of a captured,
// reassigned variable (#251), so base's chain may start from the parameter.
func cfvFactoryReassigned(db *gorm.DB) {
	db = db.Session(&gorm.Session{})
	base := func() *gorm.DB { return db.Model(nil) } // want `\*gorm\.DB reused: second branch from mutable root`
	base().Where("c").Find(nil)
	base().Where("d").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	func() { base().Find(nil) }()
}

// --- immutable-param caller contract ---

func cfvImmutableParam(db *gorm.DB) {
	//gormreuse:immutable-param
	branch := func(q *gorm.DB) {
		q.Where("a").Find(nil)
		q.Where("b").Find(nil)
	}
	branch(db.Where("x")) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter`
	func() { branch(nil) }()
}

func cfvImmutableParamInClosure(db *gorm.DB) {
	//gormreuse:immutable-param
	branch := func(q *gorm.DB) {
		q.Where("a").Find(nil)
		q.Where("b").Find(nil)
	}
	func() {
		branch(db.Where("x")) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter`
	}()
}

//gormreuse:immutable-param
func cfvApply(q *gorm.DB) {
	q.Where("a").Find(nil)
	q.Where("b").Find(nil)
}

func cfvImmutableParamFuncValue(db *gorm.DB) {
	f := cfvApply
	q := db.Where("x")
	func() {
		f(q) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter`
	}()
}

// --- method values ---

type cfvRepo struct{ db *gorm.DB }

//gormreuse:immutable-return
func (r *cfvRepo) conn() *gorm.DB { return r.db.Session(&gorm.Session{}) }

func cfvMethodValue(r *cfvRepo) {
	conn := r.conn
	func() {
		q := conn()
		q.Find(nil)
		q.Count(nil) // OK: conn is the immutable-return method
	}()
}

func cfvMethodValueGo(r *cfvRepo) {
	conn := r.conn
	var g cfvGroup
	g.Go(func() error {
		q := conn()
		q.Find(nil)
		return q.Count(nil).Error // OK: conn is the immutable-return method
	})
}

// --- immutable-input on a recursive local wrapper ---

func cfvTransaction(db *gorm.DB) error {
	//gormreuse:immutable-input(callback)
	transaction := func(ctx context.Context, db *gorm.DB, callback func(ctx context.Context, db *gorm.DB) error) error {
		return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return callback(ctx, tx)
		})
	}
	return transaction(context.Background(), db, func(ctx context.Context, tx *gorm.DB) error {
		tx.Create(nil)
		tx.First(nil) // OK: transaction hands an immutable value
		return transaction(ctx, tx, func(ctx context.Context, tx1 *gorm.DB) error {
			tx1.Create(nil)
			tx1.First(nil) // OK: also through the closure's FreeVar
			return nil
		})
	})
}

// --- not resolved: the variable may hold another function ---

func cfvAssignedTwice(db *gorm.DB, c bool) {
	//gormreuse:pure
	peek := func(q *gorm.DB) {}
	if c {
		peek = func(q *gorm.DB) { q.Where("y") }
	}
	q := db.Where("x")
	peek(q)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	func() { peek(nil) }()
}

func cfvAssignedInClosure(db *gorm.DB) {
	var peek func(*gorm.DB)
	func() {
		//gormreuse:pure
		peek = func(q *gorm.DB) {}
	}()
	q := db.Where("x")
	peek(q)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func cfvAssignedInNestedClosure(db *gorm.DB) {
	//gormreuse:pure
	peek := func(q *gorm.DB) {}
	func() {
		func() {
			peek = func(q *gorm.DB) { q.Where("y") }
		}()
	}()
	q := db.Where("x")
	peek(q)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
