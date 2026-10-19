// Package purelib declares gormreuse directives that the externalpure package
// relies on from another package (TestCrossPackageDirectives). Its own
// diagnostics are checked too, so every directive here must be valid and used,
// or carry an expectation.
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
func (o *Orm) DB(ctx context.Context) *gorm.DB {
	return DB.WithContext(ctx)
}

// GetDB returns an isolated *gorm.DB.
//
//gormreuse:immutable-return
func (o *Orm) GetDB() *gorm.DB {
	return DB.Session(&gorm.Session{})
}

// Factory returns an isolated *gorm.DB.
//
//gormreuse:immutable-return
func Factory() *gorm.DB {
	return DB.Session(&gorm.Session{})
}

// Inspect reads db without polluting it.
//
//gormreuse:pure
func Inspect(db *gorm.DB) {
	_ = db
}

// Unmarked has no directive, so callers treat it as polluting db.
func Unmarked(db *gorm.DB) {
	_ = db
}

// WithTenant does not pollute db, but returns a mutable chain.
//
//gormreuse:pure
func WithTenant(db *gorm.DB, tenantID int) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID)
}

// TenantDB does not pollute db and returns an isolated value.
//
//gormreuse:pure,immutable-return
func TenantDB(db *gorm.DB, tenantID int) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", tenantID).Session(&gorm.Session{})
}

// =============================================================================
// Contracts checked only in this package (#195)
// =============================================================================

// BadPure claims to be pure but pollutes db.
//
//gormreuse:pure
func BadPure(db *gorm.DB) {
	db.Where("x = ?", 1) // want `pure function pollutes \*gorm\.DB argument by calling Where`
}

// BadImmutableReturn claims an immutable result but returns a mutable chain.
//
//gormreuse:immutable-return
func BadImmutableReturn() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return DB.Session(&gorm.Session{}).Where("x = ?", 1)
}

// ApplyFilters branches tx, so callers must pass an isolated value.
//
//gormreuse:immutable-param
func ApplyFilters(tx *gorm.DB) {
	tx.Where("a = ?", 1).Find(nil)
	tx.Where("b = ?", 2).Find(nil)
}

// WithFresh hands cb an isolated value.
//
//gormreuse:immutable-input(cb)
func WithFresh(db *gorm.DB, cb func(*gorm.DB)) {
	cb(db.Session(&gorm.Session{}))
}

// ReturnArg does not pollute db, but returns it.
//
//gormreuse:pure
func ReturnArg(db *gorm.DB) *gorm.DB {
	return db
}
