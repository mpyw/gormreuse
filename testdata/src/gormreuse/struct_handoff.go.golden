package internal

import (
	"fmt"
	"reflect"

	"gorm.io/gorm"
)

// =============================================================================
// STRUCTS HANDED ON (#341)
//
// A struct that holds a *gorm.DB with a mutable root, handed to a function or
// a method, lets the callee branch the root as helper(q) does. So the call is
// a branch of the root. These are the hand-offs that -fix already takes as
// escapes (#310), so no fix is offered for them.
// =============================================================================

type shDeps struct{ DB *gorm.DB }

type shNested struct {
	Name string
	Deps shDeps
}

type shRepo struct{ db *gorm.DB }

func shUseDeps(d shDeps) { d.DB.Where("age > ?", 1).Find(nil) }

func shUseDepsPtr(d *shDeps) { d.DB.Where("age > ?", 1).Find(nil) }

func shUseNested(n shNested) { n.Deps.DB.Find(nil) }

func shUseAny(v any) { _ = v }

func (r *shRepo) active() { r.db.Where("active").Find(nil) }

func (r *shRepo) other() {}

// DB returns the handle. It is inferred immutable-return (#308).
func (r *shRepo) DB() *gorm.DB { return r.db }

//gormreuse:pure
func shPureDeps(d shDeps) int { _ = d; return 0 }

// =============================================================================
// SHOULD REPORT
// =============================================================================

// shStructArg passes a struct literal that holds q.
func shStructArg(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	shUseDeps(shDeps{DB: q})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shStructVar passes a struct variable that holds q.
func shStructVar(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	d := shDeps{DB: q}
	shUseDeps(d)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shStructPointer passes the struct's address.
func shStructPointer(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	d := shDeps{DB: q}
	shUseDepsPtr(&d)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shFieldAddress passes the address of a nested struct that holds q.
func shFieldAddress(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	n := shNested{Deps: shDeps{DB: q}}
	shUseDepsPtr(&n.Deps)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shNestedField passes a struct that holds q in a struct field.
func shNestedField(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	shUseNested(shNested{Name: "n", Deps: shDeps{DB: q}})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shBoxed passes the struct boxed in an interface.
func shBoxed(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	shUseAny(shDeps{DB: q})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shReceiver calls a method of a struct that holds q.
func shReceiver(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	r := &shRepo{db: q}
	r.active()
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shReceiverTwice calls two methods of a struct that holds q.
func shReceiverTwice(db *gorm.DB) {
	r := &shRepo{db: db.Where("name = ?", "x")}
	r.active()
	r.other() // want `\*gorm\.DB reused: second branch from mutable root`
}

// shMethodValue calls a method value of a struct that holds q.
func shMethodValue(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	r := &shRepo{db: q}
	f := r.active
	f()
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shCapturedReceiver calls the method in a closure.
func shCapturedReceiver(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	r := &shRepo{db: q}
	func() { r.active() }()
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shGetterTwice branches q through the getter twice: each result is q.
func shGetterTwice(db *gorm.DB) {
	r := &shRepo{db: db.Where("name = ?", "x")}
	r.DB().Find(nil)
	r.DB().Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// shInLoop hands the struct on in every iteration.
func shInLoop(db *gorm.DB, xs []int) {
	q := db.Where("name = ?", "x")
	for range xs {
		shUseDeps(shDeps{DB: q}) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// =============================================================================
// SHOULD NOT REPORT
// =============================================================================

// shOnce hands the struct on once and uses q nowhere else.
func shOnce(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	shUseDeps(shDeps{DB: q})
}

// shImmutable hands on a struct that holds an immutable value.
func shImmutable(db *gorm.DB) {
	r := &shRepo{db: db.Session(&gorm.Session{})}
	r.active()
	r.active()
}

// shReplaced hands on the struct after its field is replaced.
func shReplaced(db, other *gorm.DB) {
	q := db.Where("name = ?", "x")
	d := shDeps{DB: q}
	d.DB = other.Session(&gorm.Session{})
	shUseDeps(d)
	q.Find(nil)
}

// shPure hands the struct to a pure function.
func shPure(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	shPureDeps(shDeps{DB: q})
	q.Find(nil)
}

// shPrinted formats the struct: a read-only variadic argument is not followed.
func shPrinted(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	fmt.Println(shDeps{DB: q})
	q.Find(nil)
}

// shNoGorm hands the struct to a package that does not reach gorm.
func shNoGorm(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	_ = reflect.ValueOf(shDeps{DB: q})
	q.Find(nil)
}

// shStatementParse parses a model with a Statement that holds q: the methods
// of gorm.Statement read only the configuration of its DB.
func shStatementParse(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	stmt := &gorm.Statement{DB: q}
	_ = stmt.Parse(&User{})
	q.Find(nil)
}

// shThroughReceiver calls methods of its own receiver: a field read through a
// receiver is not a mutable root.
func (r *shRepo) shThroughReceiver() {
	r.active()
	r.other()
}

// shGetterOnce branches q once through the getter.
func shGetterOnce(db *gorm.DB) {
	r := &shRepo{db: db.Where("name = ?", "x")}
	r.DB().Find(nil)
}

// shTwoFields hands on a struct that holds q twice: one branch.
func shTwoFields(db *gorm.DB) {
	q := db.Where("name = ?", "x")
	shUseTwo(shTwo{A: q, B: q})
}

type shTwo struct{ A, B *gorm.DB }

func shUseTwo(t shTwo) { _ = t }
