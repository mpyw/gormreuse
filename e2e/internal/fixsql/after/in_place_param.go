// Code generated from ../before/in_place_param.go by TestGoldenFilesE2E in the main module. DO NOT EDIT.

package after

import (
	"context"

	"gorm.io/gorm"
)

// inPlaceKey is the context key under which the fixtures of #387 hand a
// *gorm.DB on. The analysis cannot tell what a context holds, so it cannot
// prove the value immutable.
type inPlaceKey struct{}

// inPlaceApplyTenant narrows the caller's value in place.
func inPlaceApplyTenant(tx *gorm.DB) {
	tx.Where("tenant_id = ?", 7)
}

// inPlaceList relies on inPlaceApplyTenant narrowing tx in place.
// //gormreuse:immutable-param would promise an immutable tx, on which the
// tenant filter would change a copy, so the directive is not suggested
// (#387). Nor is a Session at the top of the body, which would drop the
// filter as well.
func inPlaceList(tx *gorm.DB) {
	inPlaceApplyTenant(tx)
	tx.Where("name = ?", "x").Find(&[]User{})
	tx.Where("name = ?", "y").Find(&[]User{})
}

// InPlaceList passes inPlaceList a mutable value that it read from a context.
func InPlaceList(db *gorm.DB) {
	ctx := context.WithValue(context.Background(), inPlaceKey{}, db.Model(&User{}))
	inPlaceList(ctx.Value(inPlaceKey{}).(*gorm.DB))
}

// inPlaceUnproved changes nothing in place, but its only caller passes a
// value that may be mutable. The directive would hide the reuse, so the
// parameter gets a Session at the top of the body instead (#387).
func inPlaceUnproved(tx *gorm.DB) {
	tx = tx.Session(&gorm.Session{})
	tx.Where("name = ?", "x").Find(&[]User{})
	tx.Where("name = ?", "y").Find(&[]User{})
}

// InPlaceUnproved passes inPlaceUnproved a mutable value that it read from a
// context.
func InPlaceUnproved(db *gorm.DB) {
	ctx := context.WithValue(context.Background(), inPlaceKey{}, db.Model(&User{}).Where("tenant_id = ?", 7))
	inPlaceUnproved(ctx.Value(inPlaceKey{}).(*gorm.DB))
}
