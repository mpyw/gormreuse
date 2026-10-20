package purelib

import "gorm.io/gorm"

// The functions below carry no directive. Each one that is inferred
// immutable-return (#261) exports the fact, as a declared one does.

// InferredSession returns a Session result.
func InferredSession() *gorm.DB { // want InferredSession:"gormreuse immutable-return"
	return DB.Session(&gorm.Session{})
}

// InferredOpen returns gorm.Open's result, or nil on error.
func InferredOpen() (*gorm.DB, error) { // want InferredOpen:"gormreuse immutable-return"
	db, err := gorm.Open("sqlite")
	if err != nil {
		return nil, err
	}
	return db, nil
}

// InferredChain returns another inferred function's result.
func InferredChain() *gorm.DB { // want InferredChain:"gormreuse immutable-return"
	return InferredSession()
}

// ViaBadImmutableReturn returns the result of BadImmutableReturn, whose body
// breaks the contract. This package trusts both, but another package trusts
// neither, so neither exports a fact.
func ViaBadImmutableReturn() *gorm.DB {
	return BadImmutableReturn()
}

// ViaViaBadImmutableReturn depends on BadImmutableReturn through another
// inferred function, so it exports no fact either.
func ViaViaBadImmutableReturn() *gorm.DB {
	return ViaBadImmutableReturn()
}

// NotInferred returns a chain, so it is not inferred.
func NotInferred() *gorm.DB {
	return DB.Where("x = ?", 1)
}

// Holder holds a handle that its getter returns.
type Holder struct{ Conn *gorm.DB }

// Get reads the field of its receiver, so its fact carries the read, and a
// caller reads the field of its own receiver (#342).
func (h *Holder) Get() *gorm.DB { // want Get:`gormreuse immutable-return reads0\[0\]`
	return h.Conn
}
