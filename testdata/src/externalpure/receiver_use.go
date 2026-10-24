package externalpure

import (
	"gorm.io/gorm"

	"purelib"
)

// A pure method of another package that changes its receiver's handle
// branches what the receiver holds; one that only reads it does not (#404).

func receiverChangedAcrossPackages(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	purelib.ReceiverRepo{DB: q}.Changes(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func receiverReadAcrossPackages(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	purelib.ReceiverRepo{DB: q}.Reads(nil)
	q.Find(nil)
}
