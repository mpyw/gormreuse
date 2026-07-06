// This fixture is intentionally named *_test.go to exercise the analyzer's
// -test flag behavior (test files are analyzed when -test=true, the default).
//
// TRAP: analysistest loads a package's *_test.go files as part of BOTH the
// normal package and the test variant, so every diagnostic in a *_test.go
// fixture is collected twice. This once produced 343 duplicated diagnostics in
// PR #57. Only name a fixture *_test.go when you are deliberately testing
// test-file handling (as here); otherwise use a plain .go name. See CLAUDE.md
// "Testing Strategy".
package filefilter

import "gorm.io/gorm"

// badReuseInTest is reported when -test=true (default).
func badReuseInTest(db *gorm.DB) {
	q := db.Model(&User{}).Where("test = ?", true)
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB reused: second branch from mutable root`
}
