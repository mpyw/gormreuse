package internal

import "gorm.io/gorm"

// Fixtures for issue #373: a parameter merged through a Phi with a value
// whose fix rewrote the parameter's statements. A discarded statement on the
// parameter's variable changes the caller's value in place, as on the
// parameter itself (#312), so these get no fix.

// ===== NO FIX =====

// fixMergedParamLoop: the fix of the Where("a") gave the parameter its body
// Session, and the discarded Where("b") then changed a copy.
func fixMergedParamLoop(q *gorm.DB, c bool, items []int) {
	for range items {
		if c {
			q = q.Where("a")
		} else {
			q.Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
		}
		q.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
	}
	q.Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func fixMergedParamCallsLoop(db *gorm.DB) {
	fixMergedParamLoop(db.Model(&User{}), true, []int{1, 2})
}

// fixMergedParamScope narrows the caller's value in place, after peeking at
// it. Reassigning its discarded statement on the Where("admin") merged with
// q would keep the filter inside the function, and the caller would lose it.
func fixMergedParamScope(q *gorm.DB, admin bool, tenants []int) {
	if admin {
		q = q.Where("admin")
	}
	var n int64
	q.Count(&n)
	for _, t := range tenants {
		q.Where("tenant_id = ?", t) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

func fixMergedParamCallsScope(db *gorm.DB) {
	base := db.Model(&User{})
	fixMergedParamScope(base, false, []int{7, 8})
	base.Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixMergedParamLaterLoop: the parameter merged through an if, with a
// discarded statement in a later loop.
func fixMergedParamLaterLoop(q *gorm.DB, c bool, items []int) {
	if c {
		q = q.Where("a")
	}
	q.Where("b").Find(&[]User{})
	for range items {
		q.Where("c") // want `\*gorm\.DB reused: second branch from mutable root`
	}
	q.Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func fixMergedParamCallsLaterLoop(db *gorm.DB) {
	fixMergedParamLaterLoop(db.Model(&User{}), true, []int{1, 2})
}
