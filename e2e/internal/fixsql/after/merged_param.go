// Code generated from ../before/merged_param.go by TestGoldenFilesE2E in the main module. DO NOT EDIT.

package after

import "gorm.io/gorm"

// mergedParamLoop merges the parameter with a Where("a") in a loop, and
// discards a Where("b") on the merge. Both change the caller's value in
// place, so no fix is offered (#373).
func mergedParamLoop(q *gorm.DB, c bool, items []int) {
	for range items {
		if c {
			q = q.Where("a = ?", 1)
		} else {
			q.Find(&[]User{})
		}
		q.Where("b = ?", 2)
	}
	q.Find(&[]User{})
}

// MergedParamLoop runs mergedParamLoop.
func MergedParamLoop(db *gorm.DB) {
	mergedParamLoop(db.Model(&User{}), true, []int{1, 2})
}

// mergedParamScope narrows the caller's value in place, after peeking at it.
func mergedParamScope(q *gorm.DB, admin bool, tenants []int) {
	if admin {
		q = q.Where("admin = ?", true)
	}
	q.Find(&[]User{})
	for _, t := range tenants {
		q.Where("tenant_id = ?", t)
	}
}

// MergedParamScope queries the value that mergedParamScope narrowed. The
// fix of the Where("admin") must not move the tenant filter out of it (#373).
func MergedParamScope(db *gorm.DB) {
	base := db.Model(&User{})
	mergedParamScope(base, false, []int{7, 8})
	base.Find(&[]User{})
}

// MergedLoopAfterIf is mergedParamLoop on a local root. Its fix reassigns
// the Where("b") too, in one pass (#373).
func MergedLoopAfterIf(db *gorm.DB) {
	q := db.Model(&User{}).Session(&gorm.Session{})
	c := true
	for range []int{1, 2} {
		if c {
			q = q.Where("a = ?", 1).Session(&gorm.Session{})
		} else {
			q.Find(&[]User{})
		}
		q = q.Where("b = ?", 2).Session(&gorm.Session{})
	}
	q.Find(&[]User{})
}

// MergedLaterLoop reads the Phi of a loop on each iteration of a later loop.
// The Where("b") on its edge gets a Session in the same pass (#373).
func MergedLaterLoop(db *gorm.DB) {
	mergedLaterLoop(db, false, 2)
}

func mergedLaterLoop(db *gorm.DB, c bool, n int) {
	q := db.Model(&User{}).Session(&gorm.Session{})
	if c {
		q = q.Where("a = ?", 1).Session(&gorm.Session{})
	} else {
		q.Find(&[]User{})
	}
	for range n {
		q = q.Where("b = ?", 2).Session(&gorm.Session{})
	}
	for i := range 2 {
		q.Where("i = ?", i).Find(&[]User{})
	}
}

// MergedInnerLoop reads the Where("r") of the inner loop's iteration before.
// It gets a Session in the same pass (#373).
func MergedInnerLoop(db *gorm.DB) {
	q := db.Model(&User{}).Session(&gorm.Session{})
	c := true
	for i := range 2 {
		q = q.Where("o = ?", i).Session(&gorm.Session{})
		for j := range 2 {
			q.Where("x = ?", 1).Find(&[]User{})
			if c {
				q = q.Where("r = ?", j).Session(&gorm.Session{})
			}
		}
	}
}
