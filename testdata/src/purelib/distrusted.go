package purelib

import "gorm.io/gorm"

// The functions below keep their contracts only because they rely on a
// function whose body breaks its contract. This package trusts them, so each
// broken contract is reported once, but another package trusts none of them,
// so none exports the fact (#314).

// BadConn breaks its immutable-return contract.
//
//gormreuse:immutable-return
func BadConn(db *gorm.DB) *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return db.Where("x")
}

// BadConnToo breaks its contract on its own, and calls BadConn too.
//
//gormreuse:immutable-return
func BadConnToo(db *gorm.DB) *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	_ = BadConn(nil)
	return db.Where("y")
}

// DeclaredViaBad returns BadConn's result.
//
//gormreuse:immutable-return
func DeclaredViaBad(db *gorm.DB) *gorm.DB {
	return BadConn(db)
}

// DeclaredViaViaBad relies on BadConn through DeclaredViaBad.
//
//gormreuse:immutable-return
func DeclaredViaViaBad(db *gorm.DB) *gorm.DB {
	return DeclaredViaBad(db)
}

// PIR keeps the pure contract, but its result is BadConn's.
//
//gormreuse:pure,immutable-return
func PIR(db *gorm.DB) *gorm.DB { // want PIR:"gormreuse pure"
	return BadConn(db.Session(&gorm.Session{}))
}

// InferredViaDeclaredViaBad is inferred in this package from DeclaredViaBad.
func InferredViaDeclaredViaBad(db *gorm.DB) *gorm.DB {
	return DeclaredViaBad(db.Session(&gorm.Session{}))
}

// DeclaredViaInferredViaBad relies on BadImmutableReturn through an inferred
// function.
//
//gormreuse:immutable-return
func DeclaredViaInferredViaBad() *gorm.DB {
	return ViaBadImmutableReturn()
}

// DeclaredNotRelying calls BadConn, but its result is a Session, so it keeps
// its contract on its own.
//
//gormreuse:immutable-return
func DeclaredNotRelying() *gorm.DB { // want DeclaredNotRelying:"gormreuse immutable-return"
	_ = BadConn(nil)
	return DB.Session(&gorm.Session{})
}

// BadInput breaks its immutable-input contract.
//
//gormreuse:immutable-input(cb)
func BadInput(db *gorm.DB, cb func(*gorm.DB)) {
	cb(db.Where("x")) // want `immutable-input\(cb\) declared but mutable \*gorm\.DB passed to callback`
}

// Fwd hands cb to BadInput.
//
//gormreuse:immutable-input(cb)
func Fwd(db *gorm.DB, cb func(*gorm.DB)) {
	BadInput(db, cb)
}

// FwdFwd relies on BadInput through Fwd.
//
//gormreuse:immutable-input(cb)
func FwdFwd(db *gorm.DB, cb func(*gorm.DB)) {
	Fwd(db, cb)
}

// InputViaBadReturn hands cb the result of BadConn.
//
//gormreuse:immutable-input(cb)
func InputViaBadReturn(db *gorm.DB, cb func(*gorm.DB)) {
	cb(BadConn(db))
}

// InputViaBadCallback hands cb the tx that BadInput hands its own callback.
//
//gormreuse:immutable-input(cb)
func InputViaBadCallback(db *gorm.DB, cb func(*gorm.DB)) {
	BadInput(db, func(tx *gorm.DB) {
		cb(tx)
	})
}

// FwdGood hands cb to WithFresh, which keeps its contract.
//
//gormreuse:immutable-input(cb)
func FwdGood(db *gorm.DB, cb func(*gorm.DB)) { // want FwdGood:`gormreuse immutable-input\(cb\)`
	WithFresh(db, cb)
}

// BadInputErr breaks its immutable-input contract.
//
//gormreuse:immutable-input(cb)
func BadInputErr(db *gorm.DB, cb func(*gorm.DB) error) {
	_ = cb(db.Where("x")) // want `immutable-input\(cb\) declared but mutable \*gorm\.DB passed to callback`
}

// InputViaSharedCallback hands cb the tx of a callback that both gorm's
// Transaction and BadInputErr call. BadInputErr may hand it a mutable value.
//
//gormreuse:immutable-input(cb)
func InputViaSharedCallback(db *gorm.DB, cb func(*gorm.DB)) {
	f := func(tx *gorm.DB) error {
		cb(tx)
		return nil
	}
	_ = db.Transaction(f)
	BadInputErr(db.Session(&gorm.Session{}), f)
}

// InputViaTransaction hands cb the tx of a Transaction callback.
//
//gormreuse:immutable-input(cb)
func InputViaTransaction(db *gorm.DB, cb func(*gorm.DB)) { // want InputViaTransaction:`gormreuse immutable-input\(cb\)`
	_ = BadConn(nil)
	_ = db.Transaction(func(tx *gorm.DB) error {
		cb(tx)
		return nil
	})
}
