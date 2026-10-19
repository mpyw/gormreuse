// Package externalpure calls functions whose gormreuse directives are declared
// in another package (purelib). The analysis of purelib exports what it found
// as facts, and this package reads them (#195).
package externalpure

import (
	"context"

	"gorm.io/gorm"

	"purelib"
)

// =============================================================================
// SHOULD NOT REPORT - immutable-return from another package
// =============================================================================

func immutableReturnMethodWithNew() {
	db := new(purelib.Orm).GetDB()
	db.Find(nil)
	db.Find(nil) // OK: GetDB is immutable-return
}

func immutableReturnMethodWithContext() {
	db := new(purelib.Orm).DB(context.Background())
	db.Find(nil)
	db.Find(nil) // OK: DB is immutable-return
}

func immutableReturnMethodWithVariable() {
	orm := &purelib.Orm{}
	db := orm.GetDB()
	db.Find(nil)
	db.Find(nil) // OK: GetDB is immutable-return
}

func immutableReturnFunction() {
	db := purelib.Factory()
	db.Find(nil)
	db.Find(nil) // OK: Factory is immutable-return
}

// =============================================================================
// SHOULD NOT REPORT - pure from another package
// =============================================================================

func pureArgument(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.Inspect(q)
	q.Find(nil) // OK: Inspect is pure, so this is the first branch
}

func pureImmutableResult(db *gorm.DB) {
	q := purelib.TenantDB(db, 1)
	q.Find(nil)
	q.Count(nil) // OK: TenantDB is pure,immutable-return
	db.Find(nil) // OK: TenantDB is pure, so db is not polluted
}

// =============================================================================
// SHOULD REPORT
// =============================================================================

func chainFromImmutableReturn() {
	q := purelib.Factory().Where("x = ?", 1) // Chain method creates mutable
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func unmarkedArgument(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.Unmarked(q) // Unmarked function pollutes q
	q.Find(nil)         // want `\*gorm\.DB reused: second branch from mutable root`
}

// A pure function may return a mutable value.
func pureMutableResult(db *gorm.DB) {
	q := purelib.WithTenant(db, 1)
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// =============================================================================
// Directives placed anywhere, and on generic functions (#195)
// =============================================================================

func sameLinePure(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.SameLinePure(q)
	q.Find(nil) // OK: SameLinePure is pure
}

func genericPure(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.GenericPure(q, 1)
	purelib.GenericPure[string](q, "a")
	q.Find(nil) // OK: GenericPure is pure
}

func genericMethodPure(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	var b purelib.Box[int]
	b.Inspect(q)
	inspect := b.Inspect
	inspect(q)
	q.Find(nil) // OK: Box.Inspect is pure
}

// =============================================================================
// Contracts the callee's body breaks are not trusted (#195)
// =============================================================================

// BadPure's body pollutes its argument, so it is not trusted as pure.
func badPureNotTrusted(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.BadPure(q)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// BadImmutableReturn returns a mutable chain, so its result is mutable.
func badImmutableReturnNotTrusted() {
	q := purelib.BadImmutableReturn()
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// BadWithFresh hands its callback a mutable value, so tx is mutable.
func badImmutableInputNotTrusted(db *gorm.DB) {
	purelib.BadWithFresh(db, func(tx *gorm.DB) {
		tx.Find(nil)
		tx.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	})
}

// A pure function that passes its argument to a pure function of another
// package that broke the contract breaks it too (#187).
//
//gormreuse:pure
func wrapsBadPure(db *gorm.DB) {
	purelib.BadPure(db) // want `pure function passes \*gorm\.DB argument to BadPure, which violates its pure contract`
}

// =============================================================================
// Caller contracts of another package (#195)
// =============================================================================

// ApplyFilters branches its parameter, so it needs an isolated value.
func immutableParamMutableArg(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.ApplyFilters(q) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of ApplyFilters`
}

func immutableParamIsolatedArg(db *gorm.DB) {
	purelib.ApplyFilters(db.Where("x = ?", 1).Session(&gorm.Session{})) // OK: isolated
}

func immutableParamUnderDefer(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	defer purelib.ApplyFilters(q) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of ApplyFilters`
}

// WithFresh hands cb an isolated value, so tx may be reused.
func immutableInputCallback(db *gorm.DB) {
	purelib.WithFresh(db, func(tx *gorm.DB) {
		tx.Find(nil)
		tx.Count(nil) // OK: tx is isolated
	})
}

// Forwarding a callback to another package's immutable-input function keeps
// the promise.
//
//gormreuse:immutable-input(cb)
func forwardsCallback(db *gorm.DB, cb func(*gorm.DB)) {
	purelib.WithFresh(db, cb)
}

func forwardedCallback(db *gorm.DB) {
	forwardsCallback(db, func(tx *gorm.DB) {
		tx.Find(nil)
		tx.Count(nil) // OK: tx is isolated
	})
}

// =============================================================================
// Returned arguments of another package's pure function (#188, #195)
// =============================================================================

// ReturnArg returns q, so a branch from its result is a branch from q.
func pureReturnedArg(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.ReturnArg(q).Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// ReturnArg returns only its argument, so an isolated argument gives an
// isolated result.
func pureReturnedIsolatedArg(db *gorm.DB) {
	q := purelib.ReturnArg(db.Session(&gorm.Session{}))
	q.Find(nil)
	q.Count(nil) // OK: q is the isolated argument
}

// ReturnArgOrChain may also return a fresh chain, so its result stays mutable
// even when its argument is isolated.
func pureReturnedArgOrChain(db *gorm.DB) {
	q := purelib.ReturnArgOrChain(db.Session(&gorm.Session{}), true)
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// A pure wrapper of ReturnArg returns its argument too.
//
//gormreuse:pure
func wrapsReturnArg(db *gorm.DB) *gorm.DB {
	return purelib.ReturnArg(db)
}

func pureWrapperReturnedArg(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	wrapsReturnArg(q).Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// =============================================================================
// Subquery arguments through another package's functions (#238, #195)
// =============================================================================

// ReturnArg returns sub itself, so the subquery is still sub's Statement.
func subqueryThroughReturnArg(db *gorm.DB) {
	db = db.Session(&gorm.Session{})
	sub := db.Model(nil).Select("id")
	x := db.Where("id IN (?)", purelib.ReturnArg(sub))
	sub.Where("age > 1").Find(nil)
	x.Find(nil) // want `\*gorm\.DB reused: mutable root passed as a query argument`
}

// TenantDB is pure and immutable-return, so its result is a fresh value that a
// later change to sub cannot reach.
func subqueryThroughImmutableReturn(db *gorm.DB) {
	db = db.Session(&gorm.Session{})
	sub := db.Model(nil).Select("id")
	x := db.Where("id IN (?)", purelib.TenantDB(sub, 1))
	sub.Where("age > 1").Find(nil)
	x.Find(nil) // OK: the subquery is isolated
}

// =============================================================================
// Another package's methods through a method value or expression (#255, #258)
// =============================================================================

// The method value's receiver holds a *gorm.DB, so the synthetic Branch$bound
// wrapper captures one. It is not analyzed as a closure of the caller, and
// the isolated argument is not reported (#255).
func boundImmutableParamIsolated(r purelib.Repo, db *gorm.DB) {
	f := r.Branch
	f(db.Session(&gorm.Session{}))
}

// A mutable argument is reported at the call through the method value.
func boundImmutableParamMutable(r purelib.Repo, db *gorm.DB) {
	f := r.Branch
	f(db.Where("x = ?", 1)) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of Branch`
}

func exprImmutableParamIsolated(r purelib.Repo, db *gorm.DB) {
	purelib.Repo.Branch(r, db.Session(&gorm.Session{}))
}

func exprImmutableParamMutable(r purelib.Repo, db *gorm.DB) {
	purelib.Repo.Branch(r, db.Where("x = ?", 1)) // want `mutable \*gorm\.DB passed to //gormreuse:immutable-param parameter of Branch`
}

func exprPure(r purelib.Repo, db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.Repo.Inspect(r, q)
	q.Find(nil) // OK: Inspect is pure
}

func exprImmutableReturn(r purelib.Repo) {
	q := purelib.Repo.Fresh(r)
	q.Find(nil)
	q.Find(nil) // OK: Fresh is immutable-return
}

func exprPointerImmutableReturn(o *purelib.Orm) {
	q := (*purelib.Orm).GetDB(o)
	q.Find(nil)
	q.Find(nil) // OK: GetDB is immutable-return
}

func exprImmutableInput(r purelib.Repo, db *gorm.DB) {
	purelib.Repo.With(r, db, func(q *gorm.DB) {
		q.Find(nil)
		q.Find(nil) // OK: With hands the callback an isolated value
	})
}
