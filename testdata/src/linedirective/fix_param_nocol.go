package linedirective

import "gorm.io/gorm"

// The //line below has no column, so every adjusted column is 0. The
// Session fix for a reused parameter must still indent from the real column
// (issue #244).

//line gen.go:10
func ExportedParamNoColumn(tx *gorm.DB) {
	tx.Find(nil)
	tx.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
