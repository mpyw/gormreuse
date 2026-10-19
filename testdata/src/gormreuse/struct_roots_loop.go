package internal

import (
	"iter"

	"gorm.io/gorm"
)

// Roots that are not SSA instructions, in loops, and fields of a struct that a
// call returns by value or by pointer (#250).

type srlApp struct {
	DB    *gorm.DB
	Other *gorm.DB
}

type srlWrap struct{ inner srlApp }

func setupSrlVal() srlApp {
	return srlApp{DB: DB.Where("x"), Other: DB.Where("y")}
}

func setupSrlPtr() *srlApp {
	return &srlApp{DB: DB.Where("x"), Other: DB.Where("y")}
}

func setupSrlWrapPtr() *srlWrap {
	return &srlWrap{inner: setupSrlVal()}
}

//gormreuse:immutable-return
func setupSrlPtrImmutable() *srlApp {
	return &srlApp{DB: DB.Session(&gorm.Session{}), Other: DB.Session(&gorm.Session{})}
}

// =============================================================================
// A struct returned by a call inside the loop is a new root each iteration.
// =============================================================================

// valueStructCalledInLoop builds the struct on every iteration: no report.
func valueStructCalledInLoop(names []string) {
	for _, n := range names {
		app := setupSrlVal()
		app.DB.Where("name = ?", n).Find(nil)
	}
}

// pointerStructCalledInLoop is the same with a pointer: no report.
func pointerStructCalledInLoop(names []string) {
	for _, n := range names {
		app := setupSrlPtr()
		app.DB.Where("name = ?", n).Find(nil)
	}
}

// valueStructCalledInRangeFunc builds the struct in the body of a
// range-over-func loop: no report.
func valueStructCalledInRangeFunc(seq iter.Seq[string]) {
	for n := range seq {
		app := setupSrlVal()
		app.DB.Where("name = ?", n).Find(nil)
	}
}

// valueStructCalledInLoopReused branches the field twice in one iteration.
func valueStructCalledInLoopReused(names []string) {
	for _, n := range names {
		app := setupSrlVal()
		app.DB.Where("name = ?", n).Find(nil)
		app.DB.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// valueStructCalledBeforeLoop is built once and branched every iteration.
func valueStructCalledBeforeLoop(names []string) {
	app := setupSrlVal()
	for _, n := range names {
		app.DB.Where("name = ?", n).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// pointerStructCalledBeforeLoop is the same with a pointer.
func pointerStructCalledBeforeLoop(names []string) {
	app := setupSrlPtr()
	for _, n := range names {
		app.DB.Where("name = ?", n).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// =============================================================================
// A *gorm.DB parameter of a range-over-func body is new on each iteration.
// =============================================================================

// rangeFuncYieldsDB derives once from each handle the iterator yields: no
// report.
func rangeFuncYieldsDB(seq iter.Seq[*gorm.DB]) {
	for q := range seq {
		q.Where("a").Find(nil)
	}
}

// rangeFuncYieldsDBReused branches the yielded handle twice.
func rangeFuncYieldsDBReused(seq iter.Seq[*gorm.DB]) {
	for q := range seq {
		q.Where("a").Find(nil)
		q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// =============================================================================
// Value and pointer results follow one rule: each field of each call is its
// own mutable root.
// =============================================================================

// valueStructReused branches a field of a struct returned by value twice.
func valueStructReused() {
	app := setupSrlVal()
	app.DB.Where("a").Find(nil)
	app.DB.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// pointerStructReused branches a field of a struct returned by pointer twice.
func pointerStructReused() {
	app := setupSrlPtr()
	app.DB.Where("a").Find(nil)
	app.DB.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// pointerStructDirect reads the field off the call: one use per call, no
// report.
func pointerStructDirect() {
	setupSrlPtr().DB.Where("a").Find(nil)
	setupSrlPtr().DB.Where("b").Find(nil)
}

// pointerStructDistinctFields uses two fields once each: no report.
func pointerStructDistinctFields() {
	app := setupSrlPtr()
	app.DB.Where("a").Find(nil)
	app.Other.Where("b").Find(nil)
}

// pointerStructNested reuses a nested value field of a returned pointer.
func pointerStructNested() {
	w := setupSrlWrapPtr()
	w.inner.DB.Where("a").Find(nil)
	w.inner.DB.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// pointerStructImmutable reads fields of an immutable-return function: no
// report.
func pointerStructImmutable() {
	app := setupSrlPtrImmutable()
	app.DB.Where("a").Find(nil)
	app.DB.Where("b").Find(nil)
}

// pointerStructReassignedField replaces the field between the uses. The second
// use sees only the store: no report.
func pointerStructReassignedField() {
	app := setupSrlPtr()
	app.DB.Where("a").Find(nil)
	app.DB = DB.Where("z")
	app.DB.Where("b").Find(nil)
}

// pointerStructReassignedOnOnePath replaces the field on one path only. The
// returned field still reaches the second use on the other path.
func pointerStructReassignedOnOnePath(c bool) {
	app := setupSrlPtr()
	app.DB.Where("a").Find(nil)
	if c {
		app.DB = DB.Where("z")
	}
	app.DB.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// pointerStructInClosure reuses a field of a returned pointer captured by a
// closure.
func pointerStructInClosure() {
	app := setupSrlPtr()
	app.DB.Where("a").Find(nil)
	f := func() { app.DB.Where("b").Find(nil) }
	f() // want `\*gorm\.DB reused: second branch from mutable root`
}

// pointerStructParam reads a field through a pointer parameter: not a root,
// as before.
func pointerStructParam(app *srlApp) {
	app.DB.Where("a").Find(nil)
	app.DB.Where("b").Find(nil)
}
