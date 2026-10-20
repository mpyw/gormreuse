package converge

import "gorm.io/gorm"

// Fixtures for issue #200: the fixed output compiles and lints clean.

// fixCompileAddError: AddError returns an error, so it is not reassigned.
func fixCompileAddError(db *gorm.DB, err error) {
	q := db.Where("x")
	q.AddError(err)
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixCompileCommit: Commit is a finisher, so its result is not reassigned.
// Commit itself is not a use (#375), so the reuse is the Find on its result.
func fixCompileCommit(db *gorm.DB) {
	q := db.Begin().Where("x")
	q.Where("a").Find(nil)
	q.Commit().Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixCompileShadow: the parameter gorm shadows the import at the Session.
func fixCompileShadow(db *gorm.DB, gorm int) {
	q := db.Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	_ = gorm
}
