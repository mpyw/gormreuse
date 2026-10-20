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

// paramTestsTenant changes tx in place and returns it. The caller here reads
// the result, but the variant without the test file cannot tell that every
// caller does, so its discarded statement gets no fix in either variant
// (#312).
func paramTestsTenant(tx *gorm.DB) *gorm.DB {
	tx.Where("a").Find(nil)
	tx.Where("tenant") // want `\*gorm\.DB reused: second branch from mutable root`
	return tx
}

func paramTestsCallsTenant(db *gorm.DB) {
	paramTestsTenant(db.Where("x")).Find(nil)
}
