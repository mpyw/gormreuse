// Package noimport tests fix generation when gorm is not directly imported.
package noimport

import (
	"fmt"

	internal "gormreuse"
)

// violationWithGroupedImport demonstrates a violation with grouped imports.
// The fix should append gorm to the existing import group.
func violationWithGroupedImport() {
	fmt.Println("test")
	q := internal.GetDB()
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}
