package internal

import (
	"context"
	"slices"

	"gorm.io/gorm"
)

// =============================================================================
// Where a closure runs: through other closures, per iteration, and every root
// of a merge (#305, #306)
//
// A closure that another closure calls through a captured variable runs where
// that closure calls it, so it runs again when the calling closure calls it
// twice or in a loop, or itself runs again. A closure made on each iteration
// of a loop is a new closure each time: calls of it on exclusive branches run
// each one once. A use in a loop reuses each root that a merge may hold, not
// only the first one.
// =============================================================================

// --- A closure called through another closure (#305) ---

func rcsCalledInLoopOfClosure(db *gorm.DB, ids []int) error {
	base := db.Model(nil).Where("name = ?", "x")
	update := func(id int) error {
		return base.Where("id = ?", id).Find(nil).Error // want `\*gorm\.DB reused: second branch from mutable root`
	}
	run := func() error {
		for _, id := range ids {
			_ = update(id)
		}
		return nil
	}
	return run()
}

func rcsCalledTwiceInTransaction(db, conn *gorm.DB) error {
	base := db.Model(nil).Where("name = ?", "x")
	update := func(id int) error {
		return base.Where("id = ?", id).Find(nil).Error // want `\*gorm\.DB reused: second branch from mutable root`
	}
	return conn.Transaction(func(tx *gorm.DB) error {
		_ = update(1)
		return update(2)
	})
}

func rcsCalledTwiceInIIFE(db *gorm.DB) {
	base := db.Model(nil).Where("name = ?", "x")
	update := func(id int) error {
		return base.Where("id = ?", id).Find(nil).Error // want `\*gorm\.DB reused: second branch from mutable root`
	}
	func() {
		_ = update(1)
		_ = update(2)
	}()
}

func rcsCalledInLoopOfGoroutine(db *gorm.DB, ids []int) {
	base := db.Model(nil).Where("name = ?", "x")
	update := func(id int) error {
		return base.Where("id = ?", id).Find(nil).Error // want `\*gorm\.DB reused: second branch from mutable root`
	}
	go func() {
		for _, id := range ids {
			_ = update(id)
		}
	}()
}

// The body of a range-over-func loop runs once per element.
func rcsCalledInRangeFunc(db *gorm.DB, ids []int) {
	base := db.Model(nil).Where("name = ?", "x")
	update := func(id int) error {
		return base.Where("id = ?", id).Find(nil).Error // want `\*gorm\.DB reused: second branch from mutable root`
	}
	for _, id := range slices.All(ids) {
		_ = update(id)
	}
}

// A closure nested two deep calls it twice.
func rcsCalledTwiceTwoDeep(db *gorm.DB) {
	base := db.Model(nil).Where("name = ?", "x")
	update := func(id int) error {
		return base.Where("id = ?", id).Find(nil).Error // want `\*gorm\.DB reused: second branch from mutable root`
	}
	outer := func() {
		inner := func() {
			_ = update(1)
			_ = update(2)
		}
		inner()
	}
	outer()
}

// A recursive closure runs again inside itself.
func rcsRecursive(db *gorm.DB, n int) {
	base := db.Model(nil).Where("name = ?", "x")
	var walk func(n int)
	walk = func(n int) {
		base.Where("n = ?", n).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		if n > 0 {
			walk(n - 1)
		}
	}
	walk(n)
}

// Two closures that call each other recurse.
func rcsMutualRecursion(db *gorm.DB, n int) {
	base := db.Model(nil).Where("name = ?", "x")
	var even, odd func(n int)
	even = func(n int) {
		base.Where("n = ?", n).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		if n > 0 {
			odd(n - 1)
		}
	}
	odd = func(n int) {
		if n > 0 {
			even(n - 1)
		}
	}
	even(n)
}

// Called once by a closure that runs once: it runs once.
func rcsCalledOnceThroughClosure(db *gorm.DB) error {
	base := db.Model(nil).Where("name = ?", "x")
	update := func(id int) error {
		return base.Where("id = ?", id).Find(nil).Error
	}
	run := func() error { return update(1) }
	return run()
}

// rcsExclusiveAcrossClosures: [LIMITATION] the closure runs once, on one of
// two exclusive branches, but its two sites are in two functions, and sites
// in two functions are taken to both run.
func rcsExclusiveAcrossClosures(db *gorm.DB, c bool) {
	base := db.Model(nil).Where("name = ?", "x")
	update := func() {
		base.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	run := func() { update() }
	if c {
		update()
	} else {
		run()
	}
}

// --- A closure made on each iteration (#306) ---

type rcsItem struct {
	ID  int
	New bool
}

func rcsPerIterationExclusive(db *gorm.DB, items []rcsItem) error {
	db = db.Session(&gorm.Session{})
	var err error
	for _, it := range items {
		q := db.Model(nil).Where("id = ?", it.ID)
		save := func() error { return q.Updates(it).Error }
		if it.New {
			err = save()
		} else {
			err = save()
		}
	}
	return err
}

func rcsPerIterationExclusiveGoDefer(db *gorm.DB, items []rcsItem) {
	db = db.Session(&gorm.Session{})
	for _, it := range items {
		q := db.Model(nil).Where("id = ?", it.ID)
		save := func() error { return q.Updates(it).Error }
		if it.New {
			go save()
		} else {
			defer save()
		}
	}
}

func rcsPerIterationTwice(db *gorm.DB, items []rcsItem) {
	db = db.Session(&gorm.Session{})
	for _, it := range items {
		q := db.Model(nil).Where("id = ?", it.ID)
		save := func() error {
			return q.Updates(it).Error // want `\*gorm\.DB reused: second branch from mutable root`
		}
		_ = save()
		_ = save()
	}
}

// A variable that keeps the closure of an earlier iteration runs it again.
func rcsPerIterationKept(db *gorm.DB, items []rcsItem) {
	db = db.Session(&gorm.Session{})
	var last func() error
	for _, it := range items {
		q := db.Model(nil).Where("id = ?", it.ID)
		save := func() error {
			return q.Updates(it).Error // want `\*gorm\.DB reused: second branch from mutable root`
		}
		if it.New {
			last = save
		}
		if last != nil {
			_ = last()
		}
	}
}

// --- Every root of a merge used in a loop (#306) ---

func rcsMergedRootsInLoop(ctx context.Context, db *gorm.DB, org *string, ids []int) {
	db = db.Session(&gorm.Session{})
	q := db.WithContext(ctx).Model(nil)
	if org != nil {
		q = q.Where("org = ?", *org)
	}
	for _, id := range ids {
		q.Where("id = ?", id).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// A merge that may hold a Session result: only the mutable root repeats.
func rcsMergedSessionInLoop(db *gorm.DB, batch int) {
	var (
		tx      = db.Order("id").Session(&gorm.Session{})
		queryDB = tx
	)
	for {
		result := queryDB.Limit(batch).Find(nil)
		if result.Error != nil {
			break
		}
		queryDB = tx.Clauses("x")
	}
}
