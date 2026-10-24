package internal

import (
	"sort"

	"gorm.io/gorm"
)

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

type prParams struct{ db *gorm.DB }

//gormreuse:pure
func (r prRepo) usep(p prParams) { _ = p }

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

// prArgumentStructFix hands q to a pure method inside an argument struct.
// The method only reads its arguments, so the struct does not escape there,
// and -fix still makes the root immutable.
func prArgumentStructFix(db *gorm.DB) {
	r := prRepo{db: db.Session(&gorm.Session{})}
	q := db.Where("x")
	p := prParams{db: q}
	r.usep(p)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prReceiverStructNoFix hands q to a pure method as its receiver, between
// two uses. The method may change q in place, so -fix offers no Session.
func prReceiverStructNoFix(db *gorm.DB) {
	q := db.Where("x")
	r := prRepo{db: q}
	q.Find(nil)
	r.list(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prDeferArgumentStructFix defers a pure method that gets q inside an
// argument struct. -fix still makes the root immutable.
func prDeferArgumentStructFix(db *gorm.DB) {
	r := prRepo{db: db.Session(&gorm.Session{})}
	q := db.Where("x")
	defer r.usep(prParams{db: q})
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prDeferReceiver defers a pure method on a struct that holds q. A deferred
// call runs after the function's other uses, so the defer is the second
// branch.
func prDeferReceiver(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	defer prRepo{db: q}.list(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Find(nil)
}

// prGoReceiver spawns a pure method on a struct that holds q. A spawned use
// is ordered after the function's other uses, so the go statement is the
// second branch.
func prGoReceiver(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	go prRepo{db: q}.list(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Find(nil)
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

// prReceiverLastNoFix hands q to a pure method as its receiver after both
// queries. The fix withholds the Session for any root a pure method branches
// through its receiver, wherever the call is.
func prReceiverLastNoFix(db *gorm.DB) {
	q := db.Where("x")
	r := prRepo{db: q}
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	r.list(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prParamReceiverNoFix stores q behind a pointer parameter, then calls a pure
// method on that pointer. The method branches q, and no fix is offered.
func prParamReceiverNoFix(db *gorm.DB, p *prRepo) {
	q := db.Where("x")
	p.db = q
	p.listP(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// prEarlyReturnFix calls a pure method on q's holder on a path that returns
// before the queries. That branch shares no path with them, so the report,
// its first branch and its fix are as without #404.
func prEarlyReturnFix(db *gorm.DB, c bool) {
	q := db.Where("x")
	r := prRepo{db: q}
	if c {
		r.list(nil)
		return
	}
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root \(root at pure_receiver\.go:186, first branch at pure_receiver\.go:192\); make the root immutable with \.Session`
}

// prSwitchFix does the same in another case of a switch.
func prSwitchFix(db *gorm.DB, k int) {
	q := db.Where("x")
	r := prRepo{db: q}
	switch k {
	case 1:
		r.list(nil)
	case 2:
		q.Find(nil)
		q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root \(root at pure_receiver\.go:198, first branch at pure_receiver\.go:204\); make the root immutable with \.Session`
	}
}

// =============================================================================
// UNSAFE FIX: A PURE CALL AFTER A STORE INTO OTHER MEMORY
//
// //gormreuse:pure covers the parameters only. -fix takes any pure call as
// one that changes nothing, so it does not see a pure call that changes q in
// place through a package-level variable, through a struct it reaches some
// other way, or through code it runs. Each fixture below gets a Session that
// drops such a change. This predates #404. Each one is a known limitation.
// =============================================================================

type prWrap struct{ inner *prRepo }

var (
	prGlobal     prRepo
	prGlobalPtr  *prRepo
	prGlobalWrap prWrap
	prItems      = []int{2, 1}
)

//gormreuse:pure
func prTouchGlobal(q *gorm.DB) int { _ = q; prGlobal.db.Where("leak"); return 0 }

//gormreuse:pure
func prNothing(q *gorm.DB) int { _ = q; return 0 }

func prMutateGlobal() { prGlobal.db.Where("leak") }

//gormreuse:pure
func (w prWrap) prApply(q *gorm.DB) int { _ = q; w.inner.db.Where("leak"); return 0 }

func prLessTouchesGlobal(i, j int) bool { prGlobal.db.Where("leak"); return prItems[i] < prItems[j] }

//gormreuse:pure
func prSortNamed(q *gorm.DB) int { _ = q; sort.Slice(prItems, prLessTouchesGlobal); return 0 }

//gormreuse:pure
func prSortLiteral(q *gorm.DB) int {
	_ = q
	sort.Slice(prItems, func(i, j int) bool { prGlobal.db.Where("leak"); return prItems[i] < prItems[j] })
	return 0
}

//gormreuse:pure
func (r *prRepo) prRegister(q *gorm.DB) int { _ = q; prGlobalPtr = r; return 0 }

func prMutateViaGlobalPtr() { prGlobalPtr.db.Where("leak") }

// [LIMITATION] prUnsafePureTouchesGlobal: the pure function changes q through
// prGlobal, and the Session drops it.
func prUnsafePureTouchesGlobal(db *gorm.DB) {
	q := db.Where("x")
	prGlobal.db = q
	prTouchGlobal(nil)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// [LIMITATION] prUnsafePureHidesNextCall: the pure call ends the search, so
// the call after it, which changes q, is not seen.
func prUnsafePureHidesNextCall(db *gorm.DB) {
	q := db.Where("x")
	prGlobal.db = q
	prNothing(nil)
	prMutateGlobal()
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// [LIMITATION] prUnsafeLocalWrapper: the receiver holds the address of r,
// which holds q.
func prUnsafeLocalWrapper(db *gorm.DB) {
	q := db.Where("x")
	r := &prRepo{db: q}
	w := prWrap{inner: r}
	w.prApply(nil)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// [LIMITATION] prUnsafeWrapperBehindPointer: the same through a wrapper a
// parameter points to.
func prUnsafeWrapperBehindPointer(db *gorm.DB, w *prWrap) {
	q := db.Where("x")
	r := prRepo{db: q}
	w.inner = &r
	w.prApply(nil)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// [LIMITATION] prUnsafeGlobalWrapper: the same through a package-level
// wrapper.
func prUnsafeGlobalWrapper(db *gorm.DB) {
	q := db.Where("x")
	r := prRepo{db: q}
	prGlobalWrap.inner = &r
	prGlobalWrap.prApply(nil)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// [LIMITATION] prUnsafeSortNamed: sort.Slice runs a named function that
// changes q through prGlobal.
func prUnsafeSortNamed(db *gorm.DB) {
	q := db.Where("x")
	prGlobal.db = q
	prSortNamed(nil)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// [LIMITATION] prUnsafeSortLiteral: the same with a function literal.
func prUnsafeSortLiteral(db *gorm.DB) {
	q := db.Where("x")
	prGlobal.db = q
	prSortLiteral(nil)
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// [LIMITATION] prUnsafeRetainedReceiver: the pure method keeps the address of
// r, so a later call changes q through it.
func prUnsafeRetainedReceiver(db *gorm.DB) {
	var r prRepo
	r.prRegister(nil)
	q := db.Where("x")
	r.db = q
	prMutateViaGlobalPtr()
	q.Find(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
