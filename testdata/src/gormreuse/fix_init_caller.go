package internal

import "gorm.io/gorm"

// Fixtures for issue #346: a call in a package-level initializer is a
// caller too. It belongs to the package's init function, which the analysis
// otherwise leaves out.

var initCallerDB *gorm.DB

// ===== FIXED =====

// initCallerCountBoth reuses its parameter. Its only caller, in the
// initializer below, passes a mutable value, so declaring the function
// immutable-param would only move the reuse there. It gets the body Session.
func initCallerCountBoth(db *gorm.DB) int64 {
	var n int64
	db.Count(&n)
	db.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	return n
}

var initCallerN = initCallerCountBoth(initCallerDB.Where("x"))

// initCallerDeclared has the directive. The initializer's mutable argument
// breaks it, and is isolated.
//
//gormreuse:immutable-param
func initCallerDeclared(db *gorm.DB) int64 {
	var n int64
	db.Count(&n)
	db.Find(nil)
	return n
}

var initCallerM = initCallerDeclared(initCallerDB.Where("x")) // want `mutable \*gorm\.DB passed to initCallerDeclared, which has a gormreuse:immutable-param directive`

// initCallerIsolated passes an isolated value, so it breaks no contract.
var initCallerK = initCallerDeclared(initCallerDB.Session(&gorm.Session{}))
