// Package noimport tests fix generation when gorm is not directly imported.
// The fix should add `import "gorm.io/gorm"` when Session is added.
package noimport

import internal "gormreuse"

// violationWithoutGormImport demonstrates a violation in a file that doesn't import gorm.
// The fix adds `.Session(&gorm.Session{})` which requires gorm import.
func violationWithoutGormImport() {
	q := internal.GetDB()
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}
