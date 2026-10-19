package internal

import "gorm.io/gorm"

// =============================================================================
// //gormreuse:immutable-return body contract: returns the analyzer can see
// through (#194)
//
// The check reports a return whose value is provably mutable. Besides a direct
// gorm chain-method call, that includes a value built inside a closure whose
// body is visible (an IIFE or a local closure), a package-level variable whose
// every assignment in the package is provably mutable, a value written to a
// named result by a deferred closure, and Session(&gorm.Session{Initialized:
// true}), which returns a clone==0 handle.
// =============================================================================

var irbDB *gorm.DB

// irbFiltered is assigned only once, by its initializer, so its value is
// provably mutable.
var irbFiltered = irbDB.Where("pkg")

// irbIsolated is assigned only immutable values.
var irbIsolated = irbDB.Session(&gorm.Session{})

// irbReassigned starts mutable but is reassigned an isolated value elsewhere,
// so it gets the benefit of the doubt.
var irbReassigned = irbDB.Where("pkg")

func irbIsolateReassigned() {
	irbReassigned = irbReassigned.Session(&gorm.Session{})
}

// irbEscaped starts mutable but its address escapes, so a store through the
// pointer cannot be seen.
var irbEscaped = irbDB.Where("pkg")

func irbTakeAddress() **gorm.DB { return &irbEscaped }

// IrbExported is exported: another package may assign it, so it gets the
// benefit of the doubt even though every assignment here is mutable.
var IrbExported = irbDB.Where("pkg")

// irbAssignedInFunc has no initializer; a function assigns it a chain.
var irbAssignedInFunc *gorm.DB

func irbAssignFiltered() { irbAssignedInFunc = irbDB.Where("fn") }

// irbNeverAssigned is only ever nil.
var irbNeverAssigned *gorm.DB

// irbStoredAddress starts mutable but its address is stored elsewhere.
var irbStoredAddress = irbDB.Where("pkg")

var irbAddressHolder = &irbStoredAddress

// IRB001: IIFE whose body returns a chain.
//
//gormreuse:immutable-return
func irbIIFE() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return func() *gorm.DB { return irbDB.Where("x") }()
}

// IRB002: IIFE that captures a local.
//
//gormreuse:immutable-return
func irbIIFECapture(db *gorm.DB) *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return func() *gorm.DB { return db.Where("x") }()
}

// IRB003: local closure called once.
//
//gormreuse:immutable-return
func irbLocalClosure() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	f := func() *gorm.DB { return irbDB.Where("x") }
	return f()
}

// IRB004: local closure that captures a local, nested in another closure.
//
//gormreuse:immutable-return
func irbNestedClosure(db *gorm.DB) *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	inner := func() *gorm.DB { return db.Where("x") }
	outer := func() *gorm.DB { return inner() }
	return outer()
}

// IRB005: package-level variable assigned only a chain.
//
//gormreuse:immutable-return
func irbGlobal() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return irbFiltered
}

// IRB006: deferred closure writes a chain to the named result.
//
//gormreuse:immutable-return
func irbDeferNamed() (r *gorm.DB) { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	r = irbDB.Session(&gorm.Session{})
	defer func() { r = r.Where("x") }()
	return
}

// IRB007: deferred closure writes a chain on one path only.
//
//gormreuse:immutable-return
func irbDeferNamedConditional(cond bool) (r *gorm.DB) { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	defer func() {
		if cond {
			r = irbDB.Where("x")
		}
	}()
	return irbDB.Session(&gorm.Session{})
}

// IRB008: Session with Initialized returns a clone==0 handle (#174).
//
//gormreuse:immutable-return
func irbInitialized() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return irbDB.Session(&gorm.Session{Initialized: true})
}

// IRB009: Initialized on an already isolated value is still clone==0.
//
//gormreuse:immutable-return
func irbInitializedAfterSession() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return irbDB.Session(&gorm.Session{}).Session(&gorm.Session{Initialized: true})
}

// IRB010: IIFE returning Session with Initialized.
//
//gormreuse:immutable-return
func irbIIFEInitialized() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return func() *gorm.DB { return irbDB.Session(&gorm.Session{Initialized: true}) }()
}

// IRB011: package-level variable assigned a chain by a function.
//
//gormreuse:immutable-return
func irbGlobalAssignedInFunc() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return irbAssignedInFunc
}

// IRB012: package-level variable on one path of a Phi.
//
//gormreuse:immutable-return
func irbGlobalPhi(cond bool) *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	r := irbDB.Session(&gorm.Session{})
	if cond {
		r = irbFiltered
	}
	return r
}

// IRB013: the isolating defer is registered on one path only.
//
//gormreuse:immutable-return
func irbDeferConditionalIsolate(cond bool) (r *gorm.DB) { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	r = irbDB.Where("x")
	if cond {
		defer func() { r = r.Session(&gorm.Session{}) }()
	}
	return
}

// IRB014: the deferred closure can return before it isolates.
//
//gormreuse:immutable-return
func irbDeferEarlyReturn(cond bool) (r *gorm.DB) { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	r = irbDB.Where("x")
	defer func() {
		if cond {
			return
		}
		r = r.Session(&gorm.Session{})
	}()
	return
}

// IRB015: local closure captured by another closure and called through it.
//
//gormreuse:immutable-return
func irbCapturedClosure() *gorm.DB { // want `immutable-return declared but function returns mutable \*gorm\.DB`
	inner := func() *gorm.DB { return irbDB.Where("x") }
	outer := func() *gorm.DB { return inner() }
	return outer()
}

// =============================================================================
// SHOULD NOT REPORT
// =============================================================================

// IRB101: IIFE that isolates.
//
//gormreuse:immutable-return
func irbIIFEIsolated() *gorm.DB {
	return func() *gorm.DB { return irbDB.Where("x").Session(&gorm.Session{}) }()
}

// IRB102: local closure returning its parameter: the caller decides.
//
//gormreuse:immutable-return
func irbClosureParam(db *gorm.DB) *gorm.DB {
	f := func(d *gorm.DB) *gorm.DB { return d }
	return f(db)
}

// IRB103: package-level variable assigned only immutable values.
//
//gormreuse:immutable-return
func irbGlobalIsolated() *gorm.DB {
	return irbIsolated
}

// IRB104: package-level variable also assigned an isolated value.
//
//gormreuse:immutable-return
func irbGlobalReassigned() *gorm.DB {
	return irbReassigned
}

// IRB105: package-level variable whose address escapes.
//
//gormreuse:immutable-return
func irbGlobalEscaped() *gorm.DB {
	return irbEscaped
}

// IRB106: exported package-level variable.
//
//gormreuse:immutable-return
func irbGlobalExported() *gorm.DB {
	return IrbExported
}

// IRB107: deferred closure isolates the named result.
//
//gormreuse:immutable-return
func irbDeferIsolates() (r *gorm.DB) {
	r = irbDB.Where("x")
	defer func() { r = r.Session(&gorm.Session{}) }()
	return
}

// IRB108: deferred closure only reads the named result.
//
//gormreuse:immutable-return
func irbDeferReads() (r *gorm.DB) {
	defer func() { _ = r.Error }()
	return irbDB.Session(&gorm.Session{})
}

// IRB109: Session with Initialized explicitly false.
//
//gormreuse:immutable-return
func irbInitializedFalse() *gorm.DB {
	return irbDB.Session(&gorm.Session{Initialized: false})
}

// IRB110: a Session config the analyzer cannot read gets the benefit of the
// doubt.
//
//gormreuse:immutable-return
func irbUnknownSession(cfg *gorm.Session) *gorm.DB {
	return irbDB.Session(cfg)
}

// IRB111: recursive local closure terminates.
//
//gormreuse:immutable-return
func irbRecursiveClosure(n int) *gorm.DB {
	var f func(int) *gorm.DB
	f = func(i int) *gorm.DB {
		if i == 0 {
			return irbDB.Session(&gorm.Session{})
		}
		return f(i - 1)
	}
	return f(n)
}

// IRB112: package-level variable that is never assigned.
//
//gormreuse:immutable-return
func irbGlobalNeverAssigned() *gorm.DB {
	return irbNeverAssigned
}

// IRB113: package-level variable whose address is stored in another one.
//
//gormreuse:immutable-return
func irbGlobalStoredAddress() *gorm.DB {
	return irbStoredAddress
}

// IRB114: local closure variable assigned twice: the analyzer does not pick
// one.
//
//gormreuse:immutable-return
func irbClosureReassigned(cond bool) *gorm.DB {
	f := func() *gorm.DB { return irbDB.Where("x") }
	if cond {
		f = func() *gorm.DB { return irbDB.Session(&gorm.Session{}) }
	}
	g := func() *gorm.DB { return f() }
	return g()
}

// IRB115: local closure variable reassigned inside another closure.
//
//gormreuse:immutable-return
func irbClosureReassignedInClosure() *gorm.DB {
	f := func() *gorm.DB { return irbDB.Where("x") }
	isolate := func() { f = func() *gorm.DB { return irbDB.Session(&gorm.Session{}) } }
	isolate()
	return f()
}

// IRB116: named helper whose body is not followed.
//
//gormreuse:immutable-return
func irbNamedHelper() *gorm.DB {
	return irbHelper()
}

func irbHelper() *gorm.DB { return irbDB.Where("x") }

// IRB117: a closure marked immutable-return is trusted by its caller and
// checked on its own, so the report lands on the closure only.
//
//gormreuse:immutable-return
func irbMarkedClosure() *gorm.DB {
	//gormreuse:immutable-return
	f := func() *gorm.DB { return irbDB.Where("x") } // want `immutable-return declared but function returns mutable \*gorm\.DB`
	return f()
}
