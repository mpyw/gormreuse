package internal

import "gorm.io/gorm"

// Fixtures for issue #200: a Session goes only after a call that returns
// *gorm.DB. A Phi edge that is a closure parameter, or the result of a call
// with several results, has no such call. The enclosing call, such as
// fixSessionTargetRun(...) below, must not get one.

func fixSessionTargetRun(db *gorm.DB, fn func(tx *gorm.DB)) { fn(db) }

func fixSessionTargetOpen() (*gorm.DB, error) { return nil, nil }

// fixSessionTargetClosureParam: the edge tx is a closure parameter. Only the
// other edge gets a Session.
func fixSessionTargetClosureParam(db *gorm.DB, c bool) {
	fixSessionTargetRun(db, func(tx *gorm.DB) {
		q := tx.Where("x")
		if c {
			q = tx
		}
		q.Where("a").Find(nil)
		q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	})
}

// fixSessionTargetTuple: the edge is the first result of a call with two
// results. `fixSessionTargetOpen().Session(...)` does not compile.
func fixSessionTargetTuple(db *gorm.DB, c bool) {
	q := db.Where("x")
	if c {
		q, _ = fixSessionTargetOpen()
	}
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
