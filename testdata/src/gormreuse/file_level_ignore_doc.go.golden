// Package internal provides test cases for gormreuse analyzer.
//
//gormreuse:ignore
package internal

import "gorm.io/gorm"

// =============================================================================
// File-level ignore via package doc comment
// All violations in this file should be suppressed
// =============================================================================

// This function would normally report violations, but file-level doc ignore suppresses them
func docFileLevelIgnoreExample1(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)  // Would normally be first use - OK
	q.Count(nil) // Would normally be VIOLATION - but ignored by file-level directive
}

// Multiple violations - all suppressed by doc comment
func docFileLevelIgnoreExample2(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)   // Would be VIOLATION
	q.Count(nil)  // Would be VIOLATION
	q.First(nil)  // Would be VIOLATION
	q.Delete(nil) // Would be VIOLATION
}

// Complex control flow - all violations suppressed
func docFileLevelIgnoreExample3(db *gorm.DB, flag bool) {
	q := db.Where("base")
	if flag {
		q.Where("a").Find(nil) // Would be VIOLATION
	} else {
		q.Where("b").Find(nil) // Would be VIOLATION
	}
	q.Count(nil) // Would be VIOLATION
}
