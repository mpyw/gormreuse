package internal

import "gorm.io/gorm"

// =============================================================================
// Calls through a method expression (#258)
//
// SSA builds T.m(r, x) as a call of a synthetic "m$thunk" wrapper whose first
// parameter is the receiver. The call stands for r.m(x): the directives of m
// apply, and its arguments line up with m's parameters after the receiver.
// =============================================================================

type exprRepo struct{}

//gormreuse:pure
func (exprRepo) pureM(q *gorm.DB) {}

//gormreuse:immutable-return
func (exprRepo) fresh(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

//gormreuse:immutable-param
func (exprRepo) branch(q *gorm.DB) {
	q.Where("a").Find(nil)
	q.Where("b").Find(nil)
}

//gormreuse:immutable-input(cb)
func (exprRepo) with(db *gorm.DB, cb func(*gorm.DB)) {
	cb(db.Session(&gorm.Session{}))
}

//gormreuse:pure
func (exprRepo) same(q *gorm.DB) *gorm.DB { return q }

func (exprRepo) leakM(q *gorm.DB) { q.Find(nil) }

//gormreuse:pure
func (exprRepo) badPure(q *gorm.DB) {
	q.Find(nil) // want `pure function pollutes \*gorm\.DB argument by calling Find`
}

// ===== SHOULD NOT REPORT =====

// exprPure: pureM is pure, so passing q does not branch it.
func exprPure(db *gorm.DB) {
	q := db.Where("x")
	exprRepo.pureM(exprRepo{}, q)
	q.Find(nil)
}

// exprFresh: fresh is immutable-return.
func exprFresh(db *gorm.DB) {
	q := exprRepo.fresh(exprRepo{}, db)
	q.Find(nil)
	q.Find(nil)
}

// exprPointerFresh: a method expression on the pointer type.
func exprPointerFresh(db *gorm.DB) {
	q := (*exprRepo).fresh(&exprRepo{}, db)
	q.Find(nil)
	q.Find(nil)
}

// exprBranchIsolated: branch gets an isolated value.
func exprBranchIsolated(db *gorm.DB) {
	exprRepo.branch(exprRepo{}, db.Session(&gorm.Session{}))
}

// exprWith: the callback gets an immutable value.
func exprWith(db *gorm.DB) {
	exprRepo.with(exprRepo{}, db, func(q *gorm.DB) {
		q.Find(nil)
		q.Find(nil)
	})
}

// exprGormMethodOnSession: a gorm method expression on an immutable value
// returns a fresh chain each time.
func exprGormMethodOnSession(db *gorm.DB) {
	q := db.Session(&gorm.Session{})
	(*gorm.DB).Find(q, nil)
	(*gorm.DB).Find(q, nil)
}

// ===== SHOULD REPORT =====

// exprBranchMutable: branch needs an isolated value.
func exprBranchMutable(db *gorm.DB) {
	exprRepo.branch(exprRepo{}, db.Session(&gorm.Session{}).Where("x")) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of branch`
}

// exprSameReturnsArg: same returns its argument, so the result shares q.
func exprSameReturnsArg(db *gorm.DB) {
	q := db.Where("x")
	exprRepo.same(exprRepo{}, q).Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// exprGormMethod: (*gorm.DB).Find(q, nil) branches q like q.Find(nil).
func exprGormMethod(db *gorm.DB) {
	q := db.Where("x")
	(*gorm.DB).Find(q, nil)
	(*gorm.DB).Find(q, nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// exprOuterLeak passes its argument to a method that is not pure. As for a
// direct call, this is reported inside exprOuterLeak only.
//
//gormreuse:pure
func exprOuterLeak(db *gorm.DB) {
	exprRepo.leakM(exprRepo{}, db) // want `pure function passes \*gorm\.DB argument to non-pure function leakM$`
}

// exprOuterFailed passes its argument to a pure method that breaks its
// contract, so exprOuterFailed breaks it too.
//
//gormreuse:pure
func exprOuterFailed(db *gorm.DB) {
	exprRepo.badPure(exprRepo{}, db) // want `pure function passes \*gorm\.DB argument to badPure, which violates its pure contract`
}

// exprOuterFailedCaller: exprOuterFailed is not trusted at its call sites.
func exprOuterFailedCaller(db *gorm.DB) {
	q := db.Where("x")
	exprOuterFailed(q)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

//gormreuse:pure
func exprPureGormMethod(db *gorm.DB) {
	(*gorm.DB).Find(db, nil) // want `pure function pollutes \*gorm\.DB argument by calling Find`
}

// exprDeferBranchMutable: the contract is checked under defer too.
func exprDeferBranchMutable(db *gorm.DB) {
	defer exprRepo.branch(exprRepo{}, db.Where("x")) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of branch`
}

//gormreuse:pure
func exprPureDeferGormMethod(db *gorm.DB) {
	defer (*gorm.DB).Find(db, nil) // want `pure function pollutes \*gorm\.DB argument by calling Find`
}

//gormreuse:pure
func exprPureSession(db *gorm.DB) {
	(*gorm.DB).Session(db, &gorm.Session{}).Find(nil)
}

// exprFuncValue: a method expression held in a variable is the same call.
func exprFuncValue(db *gorm.DB) {
	f := exprRepo.branch
	f(exprRepo{}, db.Where("x")) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of branch`
}

// exprEmbedded embeds *gorm.DB, so exprEmbedded.Find is gorm's Find.
type exprEmbedded struct{ *gorm.DB }

// exprPromoted: [LIMITATION] the thunk of a promoted method selects the
// embedded field inside its body, so the receiver the call sees is e, not
// e.DB. The tracer does not follow e to the field, so the second Find, which
// e.Find(nil) twice reports, is missed.
func exprPromoted(db *gorm.DB) {
	e := exprEmbedded{db.Where("x")}
	exprEmbedded.Find(e, nil)
	exprEmbedded.Find(e, nil) // [LIMITATION] FALSE NEGATIVE
}

// exprDiscarded: the discarded method expression changes q in place, like
// q.Where("a"), so the Find is a second branch.
func exprDiscarded(db *gorm.DB) {
	q := db.Where("x")
	(*gorm.DB).Where(q, "a")
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// exprChained: a method expression as the receiver of a chain.
func exprChained(db *gorm.DB) {
	q := db.Where("x")
	(*gorm.DB).Where(q, "a").Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

type exprIface interface{ Use(q *gorm.DB) }

// exprInterface: an interface method expression has no declared method.
func exprInterface(i exprIface, db *gorm.DB) {
	q := db.Where("x")
	exprIface.Use(i, q)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// exprBranchVar holds a method expression in a package-level variable (#183),
// so its calls take the receiver as their first argument too.
var exprBranchVar = exprRepo.branch

// exprPackageVarBranchMutable: the call through the variable is the same call.
// The fix isolates the *gorm.DB argument, not the receiver (#293).
func exprPackageVarBranchMutable(db *gorm.DB) {
	exprBranchVar(exprRepo{}, db.Where("x")) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of branch`
}
