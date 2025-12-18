package internal

import "gorm.io/gorm"

// =============================================================================
// SHOULD NOT REPORT - Ignore directives
// =============================================================================

func ignoreOnSameLine(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	q.Count(nil) //gormreuse:ignore
}

func ignoreOnPreviousLine(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	//gormreuse:ignore
	q.Count(nil)
}

func ignoreWithSpace(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	// gormreuse:ignore
	q.Count(nil)
}

func ignoreMultiple(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	q.Count(nil)  //gormreuse:ignore
	q.First(nil)  //gormreuse:ignore
	q.Delete(nil) //gormreuse:ignore
}

func ignoreWithReason(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	// gormreuse:ignore - intentional reuse for pagination
	q.Count(nil)
}

// =============================================================================
// SHOULD NOT REPORT - Function-level ignore
// =============================================================================

//gormreuse:ignore
func ignoredFunction(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)
	q.Count(nil) // Not reported - entire function ignored
	q.First(nil) // Not reported - entire function ignored
}
