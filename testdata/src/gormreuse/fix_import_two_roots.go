package internal

// Fixture for issue #200: two roots in a file that does not import gorm. Each
// diagnostic's fix carries the import edit, so applying either fix alone
// compiles.

func fixImportTwoRootsFirst() {
	q := GetDB().Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func fixImportTwoRootsSecond() {
	q := GetDB().Where("y")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
