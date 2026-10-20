// Package purelib declares gormreuse directives that the externalpure package
// relies on from another package (TestAnalyzer checks both). Its own
// diagnostics are checked too, so every directive here must be valid and used,
// or carry an expectation. The analysis exports a fact for each exported
// function with a directive (#195), and each fact is checked with a
// `Name:"..."` expectation on the function's line.
package purelib

import (
	"context"

	"gorm.io/gorm"
)

// DB is a global database connection.
var DB *gorm.DB

// Orm is a wrapper type that hands out isolated *gorm.DB values.
type Orm struct{}

// =============================================================================
// Valid directives
// =============================================================================

// DB returns an isolated *gorm.DB bound to ctx.
// This simulates the pattern: new(orm.Orm).DB(ctx)
//
//gormreuse:immutable-return
func (o *Orm) DB(ctx context.Context) *gorm.DB { // want DB:"gormreuse immutable-return"
	return DB.WithContext(ctx)
}

// GetDB returns an isolated *gorm.DB.
//
//gormreuse:immutable-return
func (o *Orm) GetDB() *gorm.DB { // want GetDB:"gormreuse immutable-return"
	return DB.Session(&gorm.Session{})
}

// Factory returns an isolated *gorm.DB.
//
//gormreuse:immutable-return
func Factory() *gorm.DB { // want Factory:"gormreuse immutable-return"
	return DB.Session(&gorm.Session{})
}

// Inspect reads db without polluting it.
//
//gormreuse:pure
func Inspect(db *gorm.DB) { // want Inspect:"gormreuse pure"
	_ = db
}

// Unmarked has no directive, so callers treat it as polluting db.
func Unmarked(db *gorm.DB) { // want Unmarked:`gormreuse finisher-only\[0\]`
	_ = db
}

// WithTenant does not pollute db, but returns a mutable chain.
//
//gormreuse:pure
func WithTenant(db *gorm.DB, tenantID int) *gorm.DB { // want WithTenant:"gormreuse pure"
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID)
}

// TenantDB does not pollute db and returns an isolated value.
//
//gormreuse:pure,immutable-return
func TenantDB(db *gorm.DB, tenantID int) *gorm.DB { // want TenantDB:"gormreuse pure immutable-return"
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID).Session(&gorm.Session{})
}

// SameLinePure carries its directive after the opening brace. Before #195 a
// caller in another package read only doc comments from the source on disk.
func SameLinePure(db *gorm.DB) { //gormreuse:pure // want SameLinePure:"gormreuse pure"
	_ = db
}

// GenericPure is a pure generic function. A caller in another package calls an
// instance of it, GenericPure[int].
//
//gormreuse:pure
func GenericPure[T any](db *gorm.DB, v T) { // want GenericPure:"gormreuse pure"
	_, _ = db, v
}

// Box is a generic type with a pure method.
type Box[T any] struct{ v T }

// Inspect reads db without polluting it.
//
//gormreuse:pure
func (b Box[T]) Inspect(db *gorm.DB) { // want Inspect:"gormreuse pure"
	_, _ = b, db
}

// =============================================================================
// Contracts the bodies break: reported here, not trusted elsewhere (#195)
// =============================================================================

// BadPure claims to be pure but pollutes db.
//
//gormreuse:pure
func BadPure(db *gorm.DB) { // want BadPure:"gormreuse pure pure-failed"
	db.Where("x = ?", 1) // want `pure function pollutes \*gorm\.DB argument by calling Where`
}

// BadImmutableReturn claims an immutable result but returns a mutable chain.
// It exports no fact, so other packages treat it as unmarked.
//
//gormreuse:immutable-return
func BadImmutableReturn() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return DB.Session(&gorm.Session{}).Where("x = ?", 1)
}

// BadWithFresh hands cb a mutable value. It exports no fact.
//
//gormreuse:immutable-input(cb)
func BadWithFresh(db *gorm.DB, cb func(*gorm.DB)) {
	cb(db.Where("x = ?", 1)) // want `immutable-input\(cb\) declared but mutable \*gorm\.DB passed to callback`
}

// =============================================================================
// Contracts that bind the caller
// =============================================================================

// ApplyFilters branches tx, so callers must pass an isolated value.
//
//gormreuse:immutable-param
func ApplyFilters(tx *gorm.DB) { // want ApplyFilters:"gormreuse immutable-param"
	tx.Where("a = ?", 1).Find(nil)
	tx.Where("b = ?", 2).Find(nil)
}

// WithFresh hands cb an isolated value.
//
//gormreuse:immutable-input(cb)
func WithFresh(db *gorm.DB, cb func(*gorm.DB)) { // want WithFresh:`gormreuse immutable-input\(cb\)`
	cb(db.Session(&gorm.Session{}))
}

// ReturnArg does not pollute db, but returns it.
//
//gormreuse:pure
func ReturnArg(db *gorm.DB) *gorm.DB { // want ReturnArg:`gormreuse pure returns\[0\] returns-only-params`
	return db
}

// ReturnArgOrChain does not pollute db, but returns it or a fresh chain.
//
//gormreuse:pure
func ReturnArgOrChain(db *gorm.DB, fresh bool) *gorm.DB { // want ReturnArgOrChain:`gormreuse pure returns\[0\]$`
	if fresh {
		return db.Session(&gorm.Session{}).Where("x = ?", 1)
	}
	return db
}

// Repo holds a handle, so a method value of it captures a *gorm.DB (#255).
type Repo struct{ DB *gorm.DB }

// Branch branches q, so callers must pass an isolated value.
//
//gormreuse:immutable-param
func (Repo) Branch(q *gorm.DB) { // want Branch:"gormreuse immutable-param"
	q.Where("a = ?", 1).Find(nil)
	q.Where("b = ?", 2).Find(nil)
}

// Inspect does not pollute q.
//
//gormreuse:pure
func (Repo) Inspect(q *gorm.DB) { // want Inspect:"gormreuse pure"
	_ = q
}

// Fresh returns an isolated value.
//
//gormreuse:immutable-return
func (r Repo) Fresh() *gorm.DB { // want Fresh:"gormreuse immutable-return"
	return r.DB.Session(&gorm.Session{})
}

// With hands cb an isolated value.
//
//gormreuse:immutable-input(cb)
func (Repo) With(db *gorm.DB, cb func(*gorm.DB)) { // want With:`gormreuse immutable-input\(cb\)`
	cb(db.Session(&gorm.Session{}))
}

// helper is unexported, so another package cannot call it and it exports no
// fact.
//
//gormreuse:pure
func helper(db *gorm.DB) {
	_ = db
}

// UsesHelper keeps helper used.
func UsesHelper(db *gorm.DB) {
	helper(db)
}
