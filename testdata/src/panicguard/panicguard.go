// Package panicguard is analyzed by TestPanicInjection with a panic injected
// into one stage at a time (#315). Without one, it has no diagnostic.
package panicguard

import "gorm.io/gorm"

//gormreuse:pure
func Peek(db *gorm.DB) {}

//gormreuse:immutable-return
func Conn(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

// ConnVia is inferred immutable-return from Conn's contract, so it is
// distrusted when Conn is.
func ConnVia(db *gorm.DB) *gorm.DB {
	return Conn(db)
}

// Fresh is inferred immutable-return.
func Fresh(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

//gormreuse:immutable-input(cb)
func Each(db *gorm.DB, cb func(*gorm.DB)) {
	cb(db.Session(&gorm.Session{}))
}

//gormreuse:immutable-param
func Branch(db *gorm.DB) {
	db.Where("a").Find(nil)
	db.Where("b").Find(nil)
}

func usePeek(db *gorm.DB) {
	q := db.Where("x")
	Peek(q)
	q.Find(nil)
}

func useEach(db *gorm.DB) {
	Each(db, func(tx *gorm.DB) {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil)
	})
}

// inTx is inferred immutable-input(fn) (#349).
func inTx(db *gorm.DB, fn func(*gorm.DB) error) error {
	return db.Transaction(fn)
}

func useInTx(db *gorm.DB) error {
	return inTx(db, func(tx *gorm.DB) error {
		tx.Where("a").Find(nil)
		return tx.Where("b").Find(nil).Error
	})
}

// isolated is inferred immutable-param (#349).
func isolated(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil)
}

func useIsolated(db *gorm.DB) {
	isolated(db.Session(&gorm.Session{}))
}
