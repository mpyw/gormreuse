//gormreuse:ignore

package internal

import "gorm.io/gorm"

// =============================================================================
// File-level ignore test
// All violations in this file should be suppressed
// =============================================================================

// This function would normally report violations, but file-level ignore suppresses them
func fileLevelIgnoreExample1(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)  // Would normally be first use - OK
	q.Count(nil) // Would normally be VIOLATION - but ignored by file-level directive
}

// Multiple violations in same function - all suppressed
func fileLevelIgnoreExample2(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)   // Would be VIOLATION
	q.Count(nil)  // Would be VIOLATION
	q.First(nil)  // Would be VIOLATION
	q.Delete(nil) // Would be VIOLATION
}

// Nested violations - all suppressed
func fileLevelIgnoreExample3(db *gorm.DB, flag bool) {
	q := db.Where("x = ?", 1)
	if flag {
		q.Find(nil) // Would be VIOLATION
	}
	q.Count(nil) // Would be VIOLATION
}

// Loop violations - all suppressed
func fileLevelIgnoreExample4(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	for i := 0; i < 10; i++ {
		q.Where("y = ?", i).Find(nil) // Would be VIOLATION
	}
	q.Count(nil) // Would be VIOLATION
}

// Goroutine violations - all suppressed
func fileLevelIgnoreExample5(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	go q.Find(nil) // Would be VIOLATION
	q.Count(nil)   // Would be VIOLATION
}

// Defer violations - all suppressed
func fileLevelIgnoreExample6(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	defer q.Find(nil) // Would be VIOLATION
	q.Count(nil)      // Would be VIOLATION
}
