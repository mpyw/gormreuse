// Code generated from ../before/go_defer_only.go by TestGoldenFilesE2E in the main module. DO NOT EDIT.

package after

import "gorm.io/gorm"

// DeferOnly reuses a root only in defer statements. The deferred Find of "y"
// runs first and adds its condition to q in place, so the deferred Find of
// "x" reads it too. The fix is a Session at the root (#309).
func DeferOnly(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
	defer q.Find(&[]User{}, "name = ?", "x")
	defer q.Find(&[]User{}, "name = ?", "y")
}
