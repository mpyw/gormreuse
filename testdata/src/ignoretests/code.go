// Package ignoretests has test files. The variant of the package without them
// cannot tell whether the test variant needs an ignore in this file, so it
// leaves the unused ignore here to the test variant, which reports it without
// a fix (#248, #348). A want comment here would fail the variant without the
// test files, so TestIgnoreFixWithTestFiles checks each variant itself.
package ignoretests

import "gorm.io/gorm"

func ignoreTestsCode(db *gorm.DB) {
	db.Find(nil) //gormreuse:ignore
}
