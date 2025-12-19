// Package filefilterskip tests file filtering with -test=false.
// Tests that:
// - Generated files are always skipped (see generated.go)
// - Test files are skipped when -test=false (see code_test.go)
package filefilterskip

import "gorm.io/gorm"

// User is a test model.
type User struct {
	ID   uint
	Name string
}

// badReuse should be reported in regular files even with -test=false.
func badReuse(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true)
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB instance reused after chain method`
}

// goodReuse properly uses Session.
func goodReuse(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true).Session(&gorm.Session{})
	q.Find(&[]User{})
	q.Count(new(int64))
}
