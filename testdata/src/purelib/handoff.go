package purelib

import "gorm.io/gorm"

// DeleteByID only runs a query on db. The fact says so, so a fix in another
// package may give its argument a Session (#347).
func DeleteByID(db *gorm.DB, id int) error { // want DeleteByID:`gormreuse finisher-only\[0\]`
	return db.Where("id = ?", id).Delete(nil).Error
}

// ApplyTenant changes db in place, so it exports no fact.
func ApplyTenant(db *gorm.DB, id int) {
	db.Where("tenant_id = ?", id)
}
