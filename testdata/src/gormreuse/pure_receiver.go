package internal

import "gorm.io/gorm"

// =============================================================================
// PURE METHODS AND THEIR RECEIVER (#404)
//
// //gormreuse:pure covers the parameters only. Its body check does not seed
// the receiver, so a pure method may branch the *gorm.DB its receiver holds.
// So a pure method call is still a branch of what its receiver holds, as for
// any method (#341). Its arguments are still only read.
// =============================================================================

type prRepo struct{ db *gorm.DB }

type prEmbed struct{ *gorm.DB }

type prOuter struct{ repo prRepo }

//gormreuse:pure
func (r prRepo) list(dst *gorm.DB) { r.db.Where("leak").Find(nil) }

//gormreuse:pure
func (r *prRepo) listP(dst *gorm.DB) { r.db.Where("leak").Find(nil) }

//gormreuse:pure
func (w prEmbed) run(dst *gorm.DB) { w.Where("leak").Find(nil) }

//gormreuse:pure
func (o prOuter) list(dst *gorm.DB) { o.repo.db.Where("leak").Find(nil) }

//gormreuse:pure
func (r prRepo) find(q *gorm.DB) int { _ = q; r.db.Find(nil); return 0 }

// =============================================================================
// SHOULD REPORT
// =============================================================================

// prValueReceiver calls a pure method on a struct that holds q.
func prValueReceiver(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	prRepo{db: q}.list(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prPointerReceiver calls a pure method through a pointer receiver.
func prPointerReceiver(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	r := &prRepo{db: q}
	r.listP(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prEmbeddedReceiver calls a pure method of a struct that embeds q.
func prEmbeddedReceiver(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	prEmbed{q}.run(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prNestedReceiver calls a pure method whose receiver holds q in a nested
// struct field.
func prNestedReceiver(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	prOuter{repo: prRepo{db: q}}.list(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prMethodValue calls a pure method through a method value.
func prMethodValue(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	list := prRepo{db: q}.list
	list(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// =============================================================================
// SHOULD NOT REPORT
// =============================================================================

// prConnectionReceiver: the receiver holds an immutable handle.
func prConnectionReceiver(db *gorm.DB) {
	r := prRepo{db: db.Session(&gorm.Session{})}
	r.list(nil)
	r.list(nil)
}

// prArgumentOnly: q is an argument of a pure method, so it is only read.
func prArgumentOnly(db *gorm.DB) {
	r := prRepo{db: db.Session(&gorm.Session{})}
	q := db.Session(&gorm.Session{}).Where("x")
	r.find(q)
	q.Find(nil)
}

// prOnce: one branch through the receiver, and no other use.
func prOnce(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	prRepo{db: q}.list(nil)
}

// prGoReceiver spawns a pure method on a struct that holds q. A spawned use
// is ordered after the function's other uses, so the go statement is the
// second branch.
func prGoReceiver(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	go prRepo{db: q}.list(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Find(nil)
}
