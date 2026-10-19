package internal

import "gorm.io/gorm"

// Fixtures for issue #200: a parameter or local variable named gorm shadows
// the import where the Session goes, so the fix imports gorm.io/gorm under
// another name.

// fixImportShadowParam: the parameter gorm shadows the import.
func fixImportShadowParam(db *gorm.DB, gorm int) {
	q := db.Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	_ = gorm
}

// fixImportShadowLocal: the local gorm shadows the import.
func fixImportShadowLocal(db *gorm.DB) {
	gorm := "shadow"
	q := db.Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	_ = gorm
}

// fixImportShadowLater: a local gorm declared after the Session does not
// shadow the import there.
func fixImportShadowLater(db *gorm.DB) {
	q := db.Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	gorm := "later"
	_ = gorm
}
