package internal

import (
	"errors"

	"gorm.io/gorm"
)

// =============================================================================
// Results the tracer cannot see through (#176)
//
// A *gorm.DB that comes out of a tuple result such as (*gorm.DB, error), a
// call through a func-typed value, or an interface method is a mutable root,
// the same as the result of an unmarked user function returning *gorm.DB.
// //gormreuse:immutable-return on a known callee still makes it immutable,
// for a tuple result too.
// =============================================================================

func opaqueTuple(db *gorm.DB) (*gorm.DB, error) {
	return db.Where("x"), nil
}

func opaqueTupleErrFirst(db *gorm.DB) (error, *gorm.DB) {
	return nil, db.Where("x")
}

func opaqueTuplePair(db *gorm.DB) (*gorm.DB, *gorm.DB) {
	return db.Session(&gorm.Session{}).Where("a"), db.Session(&gorm.Session{}).Where("b")
}

//gormreuse:immutable-return
func opaqueTupleImmutable(db *gorm.DB) (*gorm.DB, error) {
	return db.Session(&gorm.Session{}), nil
}

// The body contract is checked for the *gorm.DB result of a tuple too.
//
//gormreuse:immutable-return
func opaqueTupleImmutableBroken(db *gorm.DB) (*gorm.DB, error) { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	if db == nil {
		return nil, errors.New("no db")
	}
	return db.Where("x"), nil
}

// A pure function's tuple result follows the alias rule of #188: its *gorm.DB
// may be the argument.
//
//gormreuse:pure
func opaquePureTuple(db *gorm.DB) (*gorm.DB, error) {
	return db, nil
}

// It returns its argument or a fresh chain, in the second result.
//
//gormreuse:pure
func opaquePureTupleMaybeFresh(db *gorm.DB, c bool) (error, *gorm.DB) {
	if c {
		return nil, db.Session(&gorm.Session{}).Where("x")
	}
	return nil, db
}

type opaqueRepo interface {
	Query() *gorm.DB
	QueryErr() (*gorm.DB, error)
}

// ===== SHOULD REPORT =====

// opaqueTupleReuse: the issue's first repro.
func opaqueTupleReuse(db *gorm.DB) {
	q, _ := opaqueTuple(db.Session(&gorm.Session{}))
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaqueTupleReuseWithErr: the error is checked; q is still mutable.
func opaqueTupleReuseWithErr(db *gorm.DB) error {
	q, err := opaqueTuple(db.Session(&gorm.Session{}))
	if err != nil {
		return err
	}
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	return nil
}

// opaqueTupleSecondElement: the *gorm.DB need not be the first result.
func opaqueTupleSecondElement(db *gorm.DB) {
	_, q := opaqueTupleErrFirst(db.Session(&gorm.Session{}))
	q.Where("a").Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaqueTupleArgPolluted: a tuple helper whose result is discarded still
// pollutes its argument.
func opaqueTupleArgPolluted(db *gorm.DB) {
	q := db.Where("x")
	_, _ = opaqueTuple(q)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaqueFuncParam: a func-typed parameter is an unknown callee.
func opaqueFuncParam(db *gorm.DB, build func(*gorm.DB) *gorm.DB) {
	q := build(db.Session(&gorm.Session{}))
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaqueFuncParamTuple: a func-typed parameter with a tuple result.
func opaqueFuncParamTuple(db *gorm.DB, build func(*gorm.DB) (*gorm.DB, error)) {
	q, _ := build(db.Session(&gorm.Session{}))
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaqueFuncVarMerged: a func value picked by a branch has no static callee.
func opaqueFuncVarMerged(db *gorm.DB, c bool) {
	build := opaqueTuple
	if c {
		build = opaqueTupleImmutable
	}
	q, _ := build(db.Session(&gorm.Session{}))
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaqueInterfaceMethod: the issue's interface repro.
func opaqueInterfaceMethod(r opaqueRepo) {
	q := r.Query()
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaqueInterfaceMethodTuple: an interface method with a tuple result.
func opaqueInterfaceMethodTuple(r opaqueRepo) {
	q, _ := r.QueryErr()
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaquePureTupleAlias: r is q itself, so q.Count is q's second branch.
func opaquePureTupleAlias(db *gorm.DB) {
	q := db.Where("x")
	r, _ := opaquePureTuple(q)
	r.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// opaquePureTupleMaybeFreshReuse: the result may be a fresh mutable chain, so
// it is not immutable even when the argument is.
func opaquePureTupleMaybeFreshReuse(db *gorm.DB, c bool) {
	_, r := opaquePureTupleMaybeFresh(db.Session(&gorm.Session{}), c)
	r.Where("a").Find(nil)
	r.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// [LIMITATION] opaqueTuplePairShared: both *gorm.DB results of one call share
// the call as their root, so two independent results look like one value
// branched twice. This errs on the side of reporting.
func opaqueTuplePairShared(db *gorm.DB) {
	a, b := opaqueTuplePair(db.Session(&gorm.Session{}))
	a.Find(nil)
	b.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// ===== SHOULD NOT REPORT =====

// opaqueTupleImmutableReuse: immutable-return covers a (*gorm.DB, error) result.
func opaqueTupleImmutableReuse(db *gorm.DB) {
	q, _ := opaqueTupleImmutable(db)
	q.Where("a").Find(nil)
	q.Where("b").Find(nil)
}

// opaquePureTupleOfImmutable: a pure function that returns only its argument
// hands back an immutable value when the argument is immutable.
func opaquePureTupleOfImmutable(db *gorm.DB) {
	r, _ := opaquePureTuple(db.Session(&gorm.Session{}))
	r.Where("a").Find(nil)
	r.Where("b").Find(nil)
}

// opaqueTupleSingleUse: one branch of each result is fine.
func opaqueTupleSingleUse(db *gorm.DB) {
	q, _ := opaqueTuple(db.Session(&gorm.Session{}))
	q.Where("a").Find(nil)
}

// opaqueTupleReassignUnderCondition: q, _ = helper(q) is a reassignment of q,
// like q = helper(q), not a branch of it.
func opaqueTupleReassignUnderCondition(db *gorm.DB, c bool) {
	q := db.Where("x")
	if c {
		q, _ = opaqueTuple(q)
	}
	q.Find(nil)
}

// opaqueTupleReassignInLoop: the same, in a loop.
func opaqueTupleReassignInLoop(db *gorm.DB, items []int) {
	q := db.Where("x")
	for range items {
		q, _ = opaqueTuple(q)
	}
	q.Find(nil)
}

type opaqueHolder struct{ db *gorm.DB }

// opaqueTupleFieldRebuildInLoop: r.db, _ = helper(r.db) in a loop is the field
// form of the loop rebuild, like r.db = r.db.Where(c) (#182).
func opaqueTupleFieldRebuildInLoop(r *opaqueHolder, items []int) {
	for range items {
		r.db, _ = opaqueTuple(r.db)
	}
	r.db.Find(nil)
}

// opaqueInterfaceMethodIsolated: Session() on the result isolates it.
func opaqueInterfaceMethodIsolated(r opaqueRepo) {
	q := r.Query().Session(&gorm.Session{})
	q.Where("a").Find(nil)
	q.Where("b").Find(nil)
}

// opaqueFuncParamFreshEachCall: each call is its own root.
func opaqueFuncParamFreshEachCall(db *gorm.DB, build func(*gorm.DB) *gorm.DB) {
	s := db.Session(&gorm.Session{})
	build(s).Where("a").Find(nil)
	build(s).Where("b").Find(nil)
}
