package internal

import "gorm.io/gorm"

// Fixture for issue #246: every diagnostic of a root carries the root's fix.
// Before, only the first diagnostic of a root did, and the root was marked
// fixed even when an ignore directive suppressed that diagnostic, so its fix
// was lost. A root with an ignored diagnostic now gets no fix at all (#312,
// see fix_ignored_root.go).

// fixEveryReuse: both reuses of q carry the same fix, so an editor offers it
// on either one.
func fixEveryReuse(db *gorm.DB) {
	q := db.Where("x")
	q.Find(nil)
	q.Count(nil)       // want `\*gorm\.DB reused: second branch from mutable root`
	q.Pluck("id", nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
