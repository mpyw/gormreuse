// Code generated from ../before/closure_param.go by TestGoldenFilesE2E in the main module. DO NOT EDIT.

package after

import "gorm.io/gorm"

// withTenantUsers hands its callback the users of a tenant, as a Transaction
// wrapper does. The callback's parameter is a mutable root.
func withTenantUsers(db *gorm.DB, fn func(tx *gorm.DB)) {
	fn(db.Model(&User{}).Where("tenant_id = ?", 7))
}

// ClosureParam reuses the parameter of a callback. The fix rebinds it at the
// top of the closure's body, as for a function's parameter (#313).
func ClosureParam(db *gorm.DB) {
	withTenantUsers(db, func(tx *gorm.DB) {
		tx = tx.Session(&gorm.Session{})
		tx.Where("name = ?", "x").Find(&[]User{})
		tx.Where("name = ?", "y").Find(&[]User{})
	})
}

// ClosureParamIIFE is ClosureParam with a closure called where it is written.
func ClosureParamIIFE(db *gorm.DB) {
	func(tx *gorm.DB) {
		tx = tx.Session(&gorm.Session{})
		tx.Where("name = ?", "x").Find(&[]User{})
		tx.Where("name = ?", "y").Find(&[]User{})
	}(db.Model(&User{}).Where("tenant_id = ?", 7))
}

// ClosureParamPaginate is Paginate in a closure. The Where in the if gets a
// Session of its own too (#311, #313).
func ClosureParamPaginate(db *gorm.DB) {
	paginateIn := func(tx *gorm.DB, name string) {
		tx = tx.Session(&gorm.Session{})
		var total int64
		if name != "" {
			tx = tx.Where("name = ?", name).Session(&gorm.Session{})
		}
		tx.Count(&total)
		tx.Offset(10).Limit(10).Find(&[]User{})
	}
	paginateIn(db.Model(&User{}), "x")
}
