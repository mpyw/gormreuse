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
