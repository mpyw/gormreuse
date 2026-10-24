package externalpure

import (
	"gorm.io/gorm"

	"purelib"
)

// =============================================================================
// A PURE FUNCTION THAT CALLS INTO ANOTHER PACKAGE (#404)
//
// A function of another package cannot name a package-level variable of this
// one. But when its package reaches gorm, it may run a callback this package
// registered earlier, which changes the chain the variable holds. So -fix
// offers no Session after a store into the variable.
// =============================================================================

type phRepo struct{ db *gorm.DB }

var phGlobal phRepo

func init() { purelib.RegisterHook(func() { phGlobal.db.Where("leak") }) }

//gormreuse:pure
func phRunHooks(q *gorm.DB) int {
	_ = q
	purelib.RunHooks()
	return 0
}

//gormreuse:pure
func phLength(q *gorm.DB) int {
	_ = q
	return len("x")
}

// phHooksNoFix: phRunHooks may run the hook, which changes q in place. No fix.
func phHooksNoFix(db *gorm.DB) {
	q := db.Where("x")
	phGlobal.db = q
	phRunHooks(nil)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phNoCallFix: phLength calls nothing that runs code. The fix stays.
func phNoCallFix(db *gorm.DB) {
	q := db.Where("x")
	phGlobal.db = q
	phLength(nil)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
