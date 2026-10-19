package internal

import (
	"slices"
	"testing"

	"gorm.io/gorm"
)

// TestFixCapturedMergeNeedsEveryStore checks the fix of #247 for a variable
// that a closure captures and that two paths store to. The old fix put a
// Session on the first store only, so on the other path the second query
// still saw the first one's condition. The fix now puts one on each store.
func TestFixCapturedMergeNeedsEveryStore(t *testing.T) {
	// run takes the path of the second store, with or without its Session.
	run := func(sessionOnSecond bool) func(*gorm.DB) {
		return func(db *gorm.DB) {
			var q *gorm.DB
			if sessionOnSecond {
				q = db.Model(&User{}).Where("b = 1").Session(&gorm.Session{})
			} else {
				q = db.Model(&User{}).Where("b = 1")
			}
			f := func() {
				q.Where("x = 1").Find(&[]User{})
				q.Where("y = 1").Find(&[]User{})
			}
			f()
		}
	}

	if leaked := fixsqlQueries(t, run(false))[1]; !slices.Contains(leaked, "x = 1") {
		t.Errorf("with no Session on the second store, the second query is %q, want it to see x = 1", leaked)
	}
	if fixed := fixsqlQueries(t, run(true))[1]; slices.Contains(fixed, "x = 1") {
		t.Errorf("with a Session on the second store, the second query is %q, want no x = 1", fixed)
	}
}
