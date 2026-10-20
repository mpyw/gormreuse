package externalpure

import (
	"gorm.io/gorm"

	"purelib"
)

// purelib.DeleteByID only runs a query on q, as its fact says, so q keeps
// its fix (#347).
func finisherOnlyAcrossPackages(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	_ = purelib.DeleteByID(q, 7)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// purelib.ApplyTenant changes q in place: no fix.
func changedAcrossPackages(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	purelib.ApplyTenant(q, 7)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
