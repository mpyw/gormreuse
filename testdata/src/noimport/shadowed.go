package noimport

import internal "gormreuse"

// violationWithGormParam has a parameter named gorm, so the import the fix
// adds cannot be named gorm here.
func violationWithGormParam(gorm string) {
	q := internal.GetQuery().Where("x = ?", gorm)
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// violationBesideGormParam is in the same file, so its fix adds the same
// import under the same name, not a second import of gorm.io/gorm (#378).
func violationBesideGormParam() {
	q := internal.GetQuery().Where("y")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
