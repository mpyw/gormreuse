// Package externalpure calls functions whose gormreuse directives are declared
// in another package (purelib). The analyzer exports no facts yet, so these
// directives are read back from purelib's source on disk (#195).
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
// [LIMITATION] #195 - the directive is trusted or ignored across packages
// =============================================================================

// [LIMITATION] #195: BadPure's body pollutes its argument. That is reported in
// purelib only; here the directive is still trusted.
// Expected: violation on q.Find(nil). Actual: not reported.
func badPureTrusted(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.BadPure(q)
	q.Find(nil)
}

// [LIMITATION] #195: BadImmutableReturn returns a mutable chain. That is
// reported in purelib only; here the directive is still trusted.
// Expected: violation on the second q.Find(nil). Actual: not reported.
func badImmutableReturnTrusted() {
	q := purelib.BadImmutableReturn()
	q.Find(nil)
	q.Find(nil)
}

// [LIMITATION] #195: the immutable-param caller contract is not checked for a
// callee in another package.
// Expected: `mutable *gorm.DB passed to //gormreuse:immutable-param parameter
// of ApplyFilters`. Actual: not reported.
func immutableParamMutableArg(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.ApplyFilters(q)
}

// [LIMITATION] #195: immutable-input(cb) is ignored for a callee in another
// package, so tx is treated as an ordinary mutable parameter.
// Expected: no report. Actual: false report on tx.Count(nil).
func immutableInputIgnored(db *gorm.DB) {
	purelib.WithFresh(db, func(tx *gorm.DB) {
		tx.Find(nil)
		tx.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	})
}

// [LIMITATION] #195: which parameters a pure function may return (#188) is
// known only for a callee in this package. ReturnArg returns q, but its result
// is treated as a fresh chain here.
// Expected: violation on q.Count(nil). Actual: not reported.
func pureReturnedArgUnknown(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.ReturnArg(q).Find(nil)
	q.Count(nil)
}
