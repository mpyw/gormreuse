package filefilterskip

import "gorm.io/gorm"

// badReuseInTest is NOT reported when -test=false.
func badReuseInTest(db *gorm.DB) {
	q := db.Model(&User{}).Where("test = ?", true)
	q.Find(&[]User{})
	q.Count(new(int64))
}
