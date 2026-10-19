// Package paramtests has a test file. The variant of the package without it
// cannot see the test file's calls, so a parameter root declared here gets
// the body Session in both variants, never immutable-param (#245).
package paramtests

import "gorm.io/gorm"

// paramTestsHelper gets an isolated value here and a mid-chain one in the
// test file.
func paramTestsHelper(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func paramTestsCode(db *gorm.DB) {
	paramTestsHelper(db.Session(&gorm.Session{}))
}
