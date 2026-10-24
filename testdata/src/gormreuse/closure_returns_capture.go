package internal

import "gorm.io/gorm"

// =============================================================================
// A function literal handed on that returns a captured value (#397)
//
// `run(func() *gorm.DB { return q })` hands q on, as `run(q)` or
// `run(q.Find)` does. The code that gets the literal may call it and chain on
// what it returns, as Scopes and Preload do. The literal's body makes no call
// on q, so the hand-off is the branch.
// =============================================================================

// crcRun calls fn and chains on the result.
func crcRun(fn func() *gorm.DB) {
	fn().Where("age > ?", 18).Find(nil)
}

// crcJob holds a builder that crcRunJob calls.
type crcJob struct{ build func() *gorm.DB }

// crcRunJob calls the job's builder.
func crcRunJob(j crcJob) {
	j.build().Find(nil)
}

// ===== SHOULD REPORT =====

// crcHelper: the issue's repro. run's Where changes q, and the last query
// sees it.
func crcHelper(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB { return q })
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcScopes: gorm runs the scope and the query on the value it returns.
func crcScopes(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	db.Session(&gorm.Session{}).Scopes(func(d *gorm.DB) *gorm.DB { return q }).Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcPreload: gorm runs the preload query on the value the callback returns.
func crcPreload(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	db.Session(&gorm.Session{}).Preload("Orders", func(d *gorm.DB) *gorm.DB { return q }).Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcField: the literal is stored in a struct field, and the struct is handed
// on. The store is the hand-off.
func crcField(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRunJob(crcJob{build: func() *gorm.DB { return q }})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcLocalVariable: the literal is held by a local variable before it is
// handed on.
func crcLocalVariable(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return q }
	crcRun(f)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcCapturedByReference: q is reassigned, so the literal captures the
// variable. Its value where the literal is handed on is what it returns.
func crcCapturedByReference(db *gorm.DB, cond bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	if cond {
		q = q.Where("y")
	}
	crcRun(func() *gorm.DB { return q })
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcThroughMerge: one of the literal's returns is the captured value.
func crcThroughMerge(db *gorm.DB, cond bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		if cond {
			return q
		}
		return db.Session(&gorm.Session{})
	})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcInLoop: the hand-off runs on every iteration.
func crcInLoop(db *gorm.DB, items []string) {
	q := db.Session(&gorm.Session{}).Where("x")
	for range items {
		crcRun(func() *gorm.DB { return q }) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// ===== SHOULD NOT REPORT =====

// crcOnce: one hand-off is one branch.
func crcOnce(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB { return q })
}

// crcReturnsParameter: a Scopes callback that returns its own parameter
// hands on nothing of the caller's.
func crcReturnsParameter(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	db.Session(&gorm.Session{}).Scopes(func(d *gorm.DB) *gorm.DB { return d.Where("a") }).Find(nil)
	q.Find(nil)
}

// crcReturnsSession: a Session copy has no Statement of its own to change.
func crcReturnsSession(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB { return q.Session(&gorm.Session{}) })
	q.Find(nil)
}

// crcReturnsImmutable: the captured value is immutable.
func crcReturnsImmutable(db *gorm.DB) {
	s := db.Session(&gorm.Session{})
	crcRun(func() *gorm.DB { return s })
	crcRun(func() *gorm.DB { return s })
}

// crcCalledInPlace: an IIFE is traced into its return, so the call is the
// one branch.
func crcCalledInPlace(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	func() *gorm.DB { return q }().Find(nil)
}

// crcChainInBody: the literal's own Where is the one branch of q. The value
// it returns is a new root, not q.
func crcChainInBody(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB { return q.Where("y") })
}
