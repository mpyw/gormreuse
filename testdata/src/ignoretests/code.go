// Package ignoretests has a test file. The variant of the package without it
// cannot tell whether the test variant needs an ignore in this file, so the
// unused ignore here gets no fix (#248).
package ignoretests

import "gorm.io/gorm"

func ignoreTestsCode(db *gorm.DB) {
	db.Find(nil) //gormreuse:ignore // want `unused gormreuse:ignore directive`
}
