//gormreuse:ignore

package internal

import "gorm.io/gorm"

// =============================================================================
// File-level ignore suppresses every diagnostic in the file, not only reuse
// violations (#190): contract, redundant-directive, unused-directive and
// Scopes-warning diagnostics are suppressed too.
// =============================================================================

//gormreuse:pure
func fileIgnoreContractPure(db *gorm.DB) {
	db.Where("x").Find(nil) // Would be a pure contract violation
}

//gormreuse:immutable-return
func fileIgnoreContractImmutableReturn(db *gorm.DB) *gorm.DB {
	return db.Where("x") // Would be an immutable-return contract violation
}

//gormreuse:immutable-param
func fileIgnoreContractRedundantImmutableParam(db *gorm.DB) { // Would be redundant
	db.Find(nil)
}

func fileIgnoreContractSessionInScopes(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}) // Would be a Scopes warning
	})
}

//gormreuse:pure
var fileIgnoreContractNotAFunc = 1 // Would be an unused pure directive
