package internal

import (
	"fmt"

	gorm "fakegorm"
)

// Fixture for issue #200: this file imports another package as gorm, so the
// fix imports gorm.io/gorm under another name.

var _ gorm.Session

func fixImportOther() {
	q := GetDB().Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	fmt.Println()
}
