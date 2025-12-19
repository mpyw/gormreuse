package filefilter

import "gorm.io/gorm"

// badReuseInTest is reported when -test=true (default).
func badReuseInTest(db *gorm.DB) {
	q := db.Model(&User{}).Where("test = ?", true)
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB instance reused after chain method`
}
