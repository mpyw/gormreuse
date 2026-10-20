// This fixture is intentionally named *_test.go: only the test variant, which
// sees the whole package, analyzes a function declared here, so it may be
// inferred immutable-param (#245, #349).
package paramtests

import "gorm.io/gorm"

func paramTestsCaller(db *gorm.DB) {
	paramTestsHelper(db.Where("x"))
}

// paramTestsTestHelper only gets isolated values, so it is inferred
// immutable-param, and the reuse is not reported.
func paramTestsTestHelper(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil)
}

func paramTestsTestCode(db *gorm.DB) {
	paramTestsTestHelper(db.Session(&gorm.Session{}))
}
