// Package internal contains test functions for gormreuse linter.
package internal

import "gorm.io/gorm"

// User is a test model.
type User struct {
	ID     uint
	Name   string
	Active bool
	Age    int
}

// =============================================================================
// SHOULD REPORT - Basic reuse violations
// =============================================================================

// basicReuse demonstrates unsafe reuse of a *gorm.DB after chain method.
// This case also locks the enriched diagnostic format (#76): the message names
// the mutable root and the first branch (line numbers matched loosely with \d+
// so edits above don't churn the want).
func basicReuse(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true)
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB reused: second branch from mutable root \(root at basic\.go:\d+, first branch at basic\.go:\d+\); make the root immutable with \.Session`
}

// reuseAfterChain demonstrates reuse after multiple chain methods.
func reuseAfterChain(db *gorm.DB) {
	q := db.Where("x = ?", 1).Order("id")
	q.Find(&[]User{})
	q.First(&User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// tripleUse demonstrates multiple violations from triple reuse.
func tripleUse(db *gorm.DB) {
	q := db.Model(&User{}).Where("a = ?", 1)
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB reused: second branch from mutable root`
	q.First(&User{})    // want `\*gorm\.DB reused: second branch from mutable root`
}

// sessionInMiddle demonstrates that Session in the middle doesn't make reuse safe.
func sessionInMiddle(db *gorm.DB) {
	q := db.Model(&User{}).Session(&gorm.Session{}).Where("x = ?", 1)
	q.Find(&[]User{})
	q.Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// =============================================================================
// SHOULD NOT REPORT - Safe patterns
// =============================================================================

// singleUse demonstrates safe single use of a chain result.
func singleUse(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(&[]User{}) // OK: single use
}

// sessionAtEnd demonstrates safe reuse after Session().
func sessionAtEnd(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true).Session(&gorm.Session{})
	q.Find(&[]User{})
	q.Count(new(int64)) // OK: Session at end makes it safe
}

// withContextAtEnd demonstrates safe reuse after WithContext().
func withContextAtEnd(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true).WithContext(nil)
	q.Find(&[]User{})
	q.Count(new(int64)) // OK: WithContext at end makes it safe
}

// separateChains demonstrates independent chains from the same parameter.
func separateChains(db *gorm.DB) {
	db.Where("a = ?", 1).Find(&[]User{})
	db.Where("b = ?", 2).Find(&[]User{}) // OK: separate chains
}

// separateVariables demonstrates no reuse when using different variables.
func separateVariables(db *gorm.DB) {
	q1 := db.Where("a = ?", 1)
	q2 := db.Where("b = ?", 2)

	q1.Find(nil)
	q2.Find(nil) // OK: different chains
}

// parameterDirectUse demonstrates that parameters are treated as immutable.
func parameterDirectUse(db *gorm.DB) {
	db.Where("a = ?", 1).Find(nil)
	db.Where("b = ?", 2).Find(nil) // OK: parameter is immutable source
}

// parameterMultipleChains demonstrates multiple chains from parameter.
func parameterMultipleChains(db *gorm.DB) {
	db.Where("x").Order("id").Find(nil)
	db.Where("y").Limit(10).Find(nil) // OK: independent chains
}
