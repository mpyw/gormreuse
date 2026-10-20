package internal

import "gorm.io/gorm"

// =============================================================================
// //gormreuse:pure: an argument returned inside another value, or reached
// again through an interface (#376)
//
// A pure function may return its *gorm.DB argument: callers trace the result
// to the argument (ReturnedParams). They do so only for a *gorm.DB result, so
// one returned inside a struct, a pointer, an interface or a closure would
// hide the alias. The body check reports it, and callers stop trusting the
// function.
//
// Boxing the argument in an interface does not hide it either: a type
// assertion or a method call through the interface reaches it again.
// =============================================================================

type pwHolder struct{ DB *gorm.DB }

type pwWherer interface {
	Where(query any, args ...any) *gorm.DB
}

type pwSessioner interface {
	Session(config *gorm.Session) *gorm.DB
}

// --- 1. the argument returned inside another value ---

//gormreuse:pure
func pwWrap(db *gorm.DB) pwHolder {
	return pwHolder{DB: db} // want `pure function leaks \*gorm\.DB argument via return of internal\.pwHolder`
}

//gormreuse:pure
func pwWrapPtr(db *gorm.DB) *pwHolder {
	return &pwHolder{DB: db} // want `pure function leaks \*gorm\.DB argument via return of \*internal\.pwHolder`
}

//gormreuse:pure
func pwWrapFunc(db *gorm.DB) func() *gorm.DB {
	return func() *gorm.DB { return db } // want `pure function leaks \*gorm\.DB argument via return of func\(\) \*gorm\.DB`
}

//gormreuse:pure
func pwWrapAny(db *gorm.DB) any {
	return db // want `pure function leaks \*gorm\.DB argument via return of any`
}

//gormreuse:pure
func pwAddrOf(db *gorm.DB) **gorm.DB {
	return &db // want `pure function leaks \*gorm\.DB argument via return of \*\*gorm\.DB`
}

//gormreuse:pure
func pwPassHolder(h pwHolder) pwHolder {
	return h // want `pure function leaks \*gorm\.DB argument via return of internal\.pwHolder`
}

func pwWrappedUse(db *gorm.DB) {
	q := db.Where("x")
	s := pwWrap(q)
	s.DB.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `second branch from mutable root`
}

func pwWrappedPtrUse(db *gorm.DB) {
	r := db.Where("x")
	p := pwWrapPtr(r)
	p.DB.Where("a").Find(nil)
	r.Where("b").Find(nil) // want `second branch from mutable root`
}

func pwWrappedFuncUse(db *gorm.DB) {
	t := db.Where("x")
	f := pwWrapFunc(t)
	f().Where("a").Find(nil)
	t.Where("b").Find(nil) // want `second branch from mutable root`
}

// --- 2. a result that holds no argument ---

//gormreuse:pure
func pwWrapSession(db *gorm.DB) pwHolder {
	return pwHolder{DB: db.Session(&gorm.Session{})} // OK: a fresh handle
}

//gormreuse:pure
func pwClosureUsesOnly(db *gorm.DB) func() *gorm.DB {
	return func() *gorm.DB { return db.Session(&gorm.Session{}) } // OK
}

//gormreuse:pure
func pwReturnsError(db *gorm.DB) (*gorm.DB, error) {
	return db, db.Error // OK: the argument itself, and an error
}

func pwSafeUse(db *gorm.DB) {
	q := db.Where("x")
	s := pwWrapSession(q)
	s.DB.Where("a").Find(nil)
	q.Where("b").Find(nil) // OK
}

// --- 3. the argument reached again through an interface ---

//gormreuse:pure
func pwAssert(db *gorm.DB) {
	var x any = db
	x.(*gorm.DB).Where("tenant = 1") // want `pure function pollutes \*gorm\.DB argument by calling Where`
}

//gormreuse:pure
func pwAssertOk(db *gorm.DB) {
	var x any = db
	if d, ok := x.(*gorm.DB); ok {
		d.Where("tenant = 1") // want `pure function pollutes \*gorm\.DB argument by calling Where`
	}
}

//gormreuse:pure
func pwInvoke(db *gorm.DB) {
	var i pwWherer = db
	i.Where("tenant = 1") // want `pure function pollutes \*gorm\.DB argument by calling Where`
}

//gormreuse:pure
func pwAssertInterface(db *gorm.DB) {
	var x any = db
	x.(pwWherer).Where("tenant = 1") // want `pure function passes \*gorm\.DB argument to interface method Where`
}

//gormreuse:pure
func pwInvokeSession(db *gorm.DB) *gorm.DB {
	var i pwSessioner = db
	return i.Session(&gorm.Session{}) // OK: a fresh handle
}

//gormreuse:pure
func pwErrorField(db *gorm.DB) string {
	if db.Error != nil {
		return db.Error.Error() // OK: the error field holds no argument
	}
	return ""
}

func pwInterfaceUse(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("a = 1")
	pwAssert(q)
	q.Find(nil) // want `second branch from mutable root`

	r := db.Session(&gorm.Session{}).Where("a = 1")
	pwInvoke(r)
	r.Find(nil) // want `second branch from mutable root`

	s := db.Session(&gorm.Session{}).Where("a = 1")
	pwInvokeSession(s).Find(nil)
	s.Find(nil) // OK
}
