// Code generated from ../before/phi_chain.go by TestGoldenFilesE2E in the main module. DO NOT EDIT.

package after

import "gorm.io/gorm"

// phiFilter holds the optional filters of the #343 fixtures.
type phiFilter struct{ Name, Email, Role string }

// phiAll sets every filter, so each Where runs.
var phiAll = phiFilter{Name: "x", Email: "x@example.com", Role: "admin"}

// PhiChain paginates with three conditional filters. Each filter adds a Phi
// that takes the one before. One pass gives the root and every filter a
// Session (#343).
func PhiChain(db *gorm.DB) {
	var total int64
	f := phiAll
	q := db.Model(&User{}).Session(&gorm.Session{})
	if f.Name != "" {
		q = q.Where("name = ?", f.Name).Session(&gorm.Session{})
	}
	if f.Email != "" {
		q = q.Where("email = ?", f.Email).Session(&gorm.Session{})
	}
	if f.Role != "" {
		q = q.Where("role = ?", f.Role).Session(&gorm.Session{})
	}
	q.Count(&total)
	q.Offset(10).Limit(10).Find(&[]User{})
}

// PhiChainParam is PhiChain on a parameter.
func PhiChainParam(db *gorm.DB) {
	phiChainParam(db.Model(&User{}), phiAll)
}

func phiChainParam(db *gorm.DB, f phiFilter) {
	db = db.Session(&gorm.Session{})
	var total int64
	if f.Name != "" {
		db = db.Where("name = ?", f.Name).Session(&gorm.Session{})
	}
	if f.Email != "" {
		db = db.Where("email = ?", f.Email).Session(&gorm.Session{})
	}
	if f.Role != "" {
		db = db.Where("role = ?", f.Role).Session(&gorm.Session{})
	}
	db.Count(&total)
	db.Offset(10).Limit(10).Find(&[]User{})
}

// PhiChainLoop has a loop between the filters. The diagnostic's root is the
// last filter, and the fix follows the Phis back to the root.
func PhiChainLoop(db *gorm.DB) {
	var total int64
	f := phiAll
	q := db.Model(&User{}).Session(&gorm.Session{})
	if f.Name != "" {
		q = q.Where("name = ?", f.Name).Session(&gorm.Session{})
	}
	for _, id := range []int{1, 2} {
		q = q.Where("id <> ?", id).Session(&gorm.Session{})
	}
	if f.Role != "" {
		q = q.Where("role = ?", f.Role).Session(&gorm.Session{})
	}
	q.Count(&total)
	q.Offset(10).Limit(10).Find(&[]User{})
}

// PhiChainParamLoop is PhiChainLoop on a parameter, which gets its Session
// at the top of the body in the same pass.
func PhiChainParamLoop(db *gorm.DB) {
	phiChainParamLoop(db.Model(&User{}), phiAll)
}

func phiChainParamLoop(db *gorm.DB, f phiFilter) {
	db = db.Session(&gorm.Session{})
	var total int64
	if f.Name != "" {
		db = db.Where("name = ?", f.Name).Session(&gorm.Session{})
	}
	for _, id := range []int{1, 2} {
		db = db.Where("id <> ?", id).Session(&gorm.Session{})
	}
	if f.Role != "" {
		db = db.Where("role = ?", f.Role).Session(&gorm.Session{})
	}
	db.Count(&total)
	db.Offset(10).Limit(10).Find(&[]User{})
}

// phiApplyTenant adds the tenant filter to db in place.
func phiApplyTenant(db *gorm.DB) {
	db.Where("tenant_id = ?", 7)
}

// PhiChainHelper hands the merged value to a helper before the loop. The
// fix of the last filter's root follows the Phis back only to the loop: a
// Session on the values before it would make the helper add the tenant to a
// copy (#243).
func PhiChainHelper(db *gorm.DB) {
	var total int64
	f := phiAll
	q := db.Model(&User{})
	if f.Name != "" {
		q = q.Where("name = ?", f.Name)
	}
	phiApplyTenant(q)
	for _, id := range []int{1, 2} {
		q = q.Where("id <> ?", id).Session(&gorm.Session{})
	}
	if f.Role != "" {
		q = q.Where("role = ?", f.Role).Session(&gorm.Session{})
	}
	q.Count(&total)
	q.Offset(10).Limit(10).Find(&[]User{})
}
