package external_pure_test

import (
	"context"

	"purelib"
)

// =============================================================================
// SHOULD NOT REPORT - External package pure method (new(T).Method pattern)
// =============================================================================

// externalPureMethodWithNew demonstrates that new(T).Method() pattern works
// with pure directive from external package.
func externalPureMethodWithNew() {
	db := new(purelib.Orm).GetDB()
	db.Find(nil)
	db.Find(nil) // OK: pure method from external package returns immutable
}

// externalPureMethodWithNewAndContext demonstrates new(T).DB(ctx) pattern
// from external package.
func externalPureMethodWithNewAndContext() {
	ctx := context.Background()
	db := new(purelib.Orm).DB(ctx)
	db.Find(nil)
	db.Find(nil) // OK: pure method from external package with ctx returns immutable
}

// externalPureMethodWithVariable demonstrates pure method via variable.
func externalPureMethodWithVariable() {
	orm := &purelib.Orm{}
	db := orm.GetDB()
	db.Find(nil)
	db.Find(nil) // OK: pure method from external package returns immutable
}

// externalPureFunction demonstrates pure function from external package.
func externalPureFunction() {
	db := purelib.PureFactory()
	db.Find(nil)
	db.Find(nil) // OK: pure function from external package returns immutable
}

// =============================================================================
// SHOULD REPORT - Non-pure usage (chain methods pollute)
// =============================================================================

// externalChainReuse demonstrates that chain methods still pollute.
func externalChainReuse() {
	db := new(purelib.Orm).GetDB()
	q := db.Where("x = ?", 1) // Chain method creates mutable
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB instance reused after chain method`
}
