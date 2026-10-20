// Code generated from ../before/helper_result.go by TestGoldenFilesE2E in the main module. DO NOT EDIT.

package after

import "gorm.io/gorm"

// tenantDiscarded changes tx in place and returns it, but its caller discards
// the result, so its discarded statement is not reassigned (#312).
func tenantDiscarded(tx *gorm.DB, out *[]User) *gorm.DB {
	tx.Where("age > ?", 20).Find(out)
	tx.Where("tenant_id = ?", 7)
	return tx
}

// HelperResultDiscarded relies on tenantDiscarded adding the tenant filter to
// q in place (#312).
func HelperResultDiscarded(db *gorm.DB) {
	q := db.Model(&User{}).Where("name = ?", "x")
	tenantDiscarded(q, &[]User{})
	q.Find(&[]User{})
}

// tenantUsed is tenantDiscarded with a caller that reads its result, which
// carries the change back, so its discarded statement is reassigned (#312).
func tenantUsed(tx *gorm.DB, out *[]User) *gorm.DB {
	tx = tx.Session(&gorm.Session{})
	tx.Where("age > ?", 20).Find(out)
	tx = tx.Where("tenant_id = ?", 7)
	return tx
}

// HelperResultUsed runs a query on the result of tenantUsed.
func HelperResultUsed(db *gorm.DB) {
	tenantUsed(db.Model(&User{}).Where("name = ?", "x"), &[]User{}).Find(&[]User{})
}
