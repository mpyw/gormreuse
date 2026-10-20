package purelib

import "gorm.io/gorm"

// declaredHelper is called only by DeclaredEntry. A declared immutable-param
// parameter does not prove the call, so declaredHelper is not inferred, and
// its reuse is reported (#371). Before, it was inferred through the
// directive, and the reuse of a mid-chain value that a caller in another
// package passes to DeclaredEntry went unreported.
func declaredHelper(tx *gorm.DB) {
	tx.Where("a = 1").Find(nil)
	tx.Where("b = 1").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// DeclaredEntry does not branch its parameter itself, so its directive
// changes nothing: without it, declaredHelper is reported all the same.
//
//gormreuse:immutable-param
func DeclaredEntry(tx *gorm.DB) { // want `redundant gormreuse:immutable-param directive: no \*gorm\.DB parameter is reused`
	declaredHelper(tx)
}
