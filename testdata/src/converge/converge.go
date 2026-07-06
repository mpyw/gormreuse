// Package converge holds self-contained, fully-fixable reuse scenarios for the
// convergence harness (#71): applying the suggested fix and re-linting the
// result must report nothing. Unlike the gormreuse fixtures, every violation
// here is expected to be fully resolved by its fix — no parameter roots, no
// documented limitations.
package converge

import "gorm.io/gorm"

// User is a local model.
type User struct{ ID uint }

// simpleReuse: two branches from a mutable root; fix = Session on the root.
func simpleReuse(db *gorm.DB) {
	q := db.Where("x")
	q.Find(&User{})
	q.Count(new(int64)) // want `\*gorm\.DB reused: second branch from mutable root`
}

// --- Cases that expose #71 defects (should converge once fixed) ---

// nestedArg exposes defect 1: q is reused inside base.Or(...). The correct fix
// makes q's root immutable; the buggy fix rebinds `base` and never addresses q,
// so re-linting still warns. immutable-param keeps the params off the root set so
// the only violation is the fully-fixable local q (see the package doc).
//
//gormreuse:immutable-param
func nestedArg(db, base *gorm.DB) {
	q := db.Where("base")
	base.Or(q.Where("y"))
	base.Or(q.Where("z")) // want `\*gorm\.DB reused: second branch from mutable root`
}

// multiBranchReassign exposes defect 2: after Phase-1 reassignments the
// reassignment-created (virtual) root still branches twice, so it needs a
// Session; the buggy fix emits Session only for the original root, leaving the
// re-linted output still warning.
func multiBranchReassign(db *gorm.DB) {
	q := db.Where("base")
	q.Where("a")
	q.Where("b").Find(&User{})
	q.Where("c")
	q.Where("d").Find(&User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// localBaseReuse: a local root reused via a method whose args are other chains;
// the fix Sessions the local's own root (not a rebind of anything nested).
// immutable-param keeps db off the root set so the only violation is the
// fully-fixable local base (see the package doc).
//
//gormreuse:immutable-param
func localBaseReuse(db *gorm.DB) {
	base := db.Where("start")
	base.Or(db.Where("a"))
	base.Or(db.Where("b")) // want `\*gorm\.DB reused: second branch from mutable root`
}

// reassignChain: an explicitly reassigned root that then branches twice; the
// fix must Session the reassigned (virtual) root so re-linting is clean.
func reassignChain(db *gorm.DB) {
	q := db.Where("x")
	q = q.Where("a")
	q.Find(&User{})
	q.Count(new(int64)) // want `\*gorm\.DB reused: second branch from mutable root`
}
