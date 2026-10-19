package internal

import _ "gorm.io/gorm"

// Fixture for issue #200: a blank import gives gorm.io/gorm no usable name, so
// the fix imports it again.

func fixImportBlank() {
	q := GetDB().Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
