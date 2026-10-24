package internal

import (
	"fmt"
	"hooklib"
	"log"

	"gorm.io/gorm"
)

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
	db.Session(&gorm.Session{}).Scopes(func(d *gorm.DB) *gorm.DB { crcUse(q == nil); return d }).Find(nil)
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

// ===== SHOULD REPORT (more shapes) =====

// crcMergedLiterals: the local variable holds one of two literals, and one
// returns the captured value.
func crcMergedLiterals(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return db.Session(&gorm.Session{}) }
	if c {
		f = func() *gorm.DB { return q }
	}
	crcRun(f)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcNestedCalledInPlace: the literal returns what a nested literal, called
// in place, returns.
func crcNestedCalledInPlace(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB { return func() *gorm.DB { return q }() })
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcHolder holds a query.
type crcHolder struct{ db *gorm.DB }

// crcCapturedField: the literal returns a field of a captured struct.
func crcCapturedField(db *gorm.DB) {
	h := crcHolder{db: db.Session(&gorm.Session{}).Where("x")}
	crcRun(func() *gorm.DB { return h.db })
	h.db.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcHandedOnInClosure: another closure hands the literal on, through a
// variable it captures.
func crcHandedOnInClosure(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return q }
	g := func() { crcRun(f) }
	g()
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcMadeInClosure: the literal is made and handed on in another closure,
// and captures q through it.
func crcMadeInClosure(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	g := func() { crcRun(func() *gorm.DB { return q }) }
	g()
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q = db.Session(&gorm.Session{})
}

// crcReassignedLater: q is reassigned after the hand-off, so the literal
// captures the variable. It reads the value where it is handed on.
func crcReassignedLater(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB { return q })
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q = db.Session(&gorm.Session{}).Where("y")
	q.Find(nil)
}

// crcMergedLiteralsInLoop: the variable is reassigned in a loop, so the
// merge of literals is a cycle.
func crcMergedLiteralsInLoop(db *gorm.DB, items []string) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return db.Session(&gorm.Session{}) }
	for _, item := range items {
		if item == "q" {
			f = func() *gorm.DB { return q }
		}
	}
	crcRun(f)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcLoopInLiteral: the literal's return merges values in a loop.
func crcLoopInLiteral(db *gorm.DB, items []string) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		r := q
		for _, item := range items {
			if item == "s" {
				r = db.Session(&gorm.Session{})
			}
		}
		return r
	})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcNestedLocal: a nested literal called in place returns a variable of the
// outer literal, which holds the captured value.
func crcNestedLocal(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		r := q
		return func() *gorm.DB { return r }()
	})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcSent: the literal is sent on a channel.
func crcSent(db *gorm.DB, ch chan func() *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	ch <- func() *gorm.DB { return q }
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcMapValue: the literal is stored in a map.
func crcMapValue(db *gorm.DB, m map[string]func() *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	m["q"] = func() *gorm.DB { return q }
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// ===== SHOULD NOT REPORT (more shapes) =====

// crcReassignsCapture: the literal assigns the captured variable its own
// chain and returns that. The Where in the body is the one branch of q.
func crcReassignsCapture(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		q = q.Where("z")
		return q
	})
}

// crcNestedOwnChain: a nested literal returns the outer literal's own chain,
// which the outer body's Where already branches once. The hand-off adds no
// second branch.
func crcNestedOwnChain(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		r := q.Where("y")
		return func() *gorm.DB { return r }()
	})
}

// crcRecursiveLiteral: a literal that captures itself ends the search.
func crcRecursiveLiteral(db *gorm.DB) {
	s := db.Session(&gorm.Session{})
	var f func() *gorm.DB
	f = func() *gorm.DB {
		if s == nil {
			return f()
		}
		return s
	}
	crcRun(f)
	crcRun(f)
}

// crcNestedAssigns: a nested literal called in place assigns the captured
// variable before the return, so the literal returns its own chain.
func crcNestedAssigns(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		func() { q = q.Where("z") }()
		return q
	})
}

// crcFormatted: fmt and log only format what they are passed, so a method
// value or a literal passed to them is not handed on.
func crcFormatted(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fmt.Println(q.Find)
	log.Printf("%v", func() *gorm.DB { return q })
	q.Find(nil)
}

// crcLocalBoxedSlice: a method value boxed in a local []any is not handed on.
func crcLocalBoxedSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	vs := []any{q.Find}
	_ = vs
	q.Find(nil)
}

// crcLocalSlice: a literal in a local slice that never leaves the function.
func crcLocalSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	_ = fs
	q.Find(nil)
}

// crcLocalStruct: a literal in a local struct that never leaves the function.
func crcLocalStruct(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	j := crcJob{build: func() *gorm.DB { return q }}
	_ = j
	q.Find(nil)
}

// crcLocalMap: a literal in a local map that never leaves the function.
func crcLocalMap(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	m := map[string]func() *gorm.DB{"q": func() *gorm.DB { return q }}
	_ = m["q"]
	q.Find(nil)
}

// crcLocalSliceCalled: a literal read from a local slice and called, with
// its result dropped, makes no call on q.
func crcLocalSliceCalled(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := [1]func() *gorm.DB{func() *gorm.DB { return q }}
	for _, f := range fs {
		f()
	}
	q.Find(nil)
}

// crcNamedJob holds a builder and its name.
type crcNamedJob struct {
	name  string
	build func() *gorm.DB
}

// crcLocalStructOtherField: only another field of the local struct is read.
func crcLocalStructOtherField(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	j := crcNamedJob{name: "q", build: func() *gorm.DB { return q }}
	crcUse(j.name == "")
	q.Find(nil)
}

// crcLocalSliceGoDefer: a literal read from a local slice is run by go and
// defer statements, which drop its result.
func crcLocalSliceGoDefer(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	go fs[0]()
	defer fs[0]()
	q.Find(nil)
}

// crcSelfHandedOn: a literal that hands itself on ends the search.
func crcSelfHandedOn(db *gorm.DB) {
	var g func() *gorm.DB
	g = func() *gorm.DB {
		crcRun(g)
		return db.Session(&gorm.Session{})
	}
	crcRun(g)
}

// crcVarargsInLoopIgnored: the report of a hand-off in a loop is on the
// call's line, so a directive there suppresses it.
func crcVarargsInLoopIgnored(db *gorm.DB, items []string) {
	q := db.Session(&gorm.Session{}).Where("x")
	for range items {
		crcRunV(func() *gorm.DB { return q }) //gormreuse:ignore
	}
}

// crcLocalSliceRanged: a range over a local slice that only calls its
// literals is no hand-off. Only the two queries are a reuse.
func crcLocalSliceRanged(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	for _, f := range fs {
		f()
	}
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcLocalSliceLen: len only reads the local slice.
func crcLocalSliceLen(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	crcUse(len(fs) > cap(fs))
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcLocalMapRanged: a range over a local map that only calls its literals,
// and a delete from it, are no hand-off.
func crcLocalMapRanged(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	m := map[string]func() *gorm.DB{"q": func() *gorm.DB { return q }}
	for _, f := range m {
		f()
	}
	delete(m, "q")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcAppendedCalled: a literal appended to a local slice whose elements are
// only called is no hand-off.
func crcAppendedCalled(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	var fs []func() *gorm.DB
	fs = append(fs, func() *gorm.DB { return q })
	fs[0]()
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcLocalSliceCopied: a copy of the local slice into another local
// variable, read and called, is no hand-off.
func crcLocalSliceCopied(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	var gs [][]func() *gorm.DB
	gs = append(gs, fs)
	for _, g := range gs {
		g[0]()
	}
	fmt.Println(fs)
	q.Find(nil)
}

// crcLocalSliceElemMerged: a variable that holds one of two elements, compared
// with nil and called, is no hand-off.
func crcLocalSliceElemMerged(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }, func() *gorm.DB { return q }}
	f := fs[0]
	if c {
		f = fs[1]
	}
	if f != nil {
		f()
	}
	q.Find(nil)
}

// crcGlobalDB holds a connection.
var crcGlobalDB *gorm.DB

// crcReturnsGlobal: a literal that returns a package-level variable hands on
// no captured value.
func crcReturnsGlobal() {
	crcRun(func() *gorm.DB { return crcGlobalDB })
	crcRun(func() *gorm.DB { return crcGlobalDB })
}

// crcReturnsParamField: a field read through a captured pointer parameter
// is not a root, as in the function itself.
func crcReturnsParamField(p *crcHolder) {
	crcRun(func() *gorm.DB { return p.db })
	crcRun(func() *gorm.DB { return p.db })
}

// crcCapturedFieldWritten: the literal writes the captured struct, through a
// closure it calls in place, so the struct is left out.
func crcCapturedFieldWritten(db *gorm.DB) {
	h := crcHolder{db: db.Session(&gorm.Session{}).Where("x")}
	crcRun(func() *gorm.DB {
		func() { h.db = db.Session(&gorm.Session{}) }()
		return h.db
	})
}

// crcElemFormatted: a function value read out of a local slice and passed
// to fmt is not handed on.
func crcElemFormatted(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	fmt.Println(fs[0])
	q.Find(nil)
}

// crcNamed holds another package's struct next to a builder.
type crcNamed struct {
	session *gorm.Session
	db      *gorm.DB
	build   func() *gorm.DB
}

// crcOtherPointerElems: pointers that hold no function value the function
// stored, a *gorm.DB and a struct of another package, are not followed.
func crcOtherPointerElems(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	n := crcNamed{session: &gorm.Session{}, db: db, build: func() *gorm.DB { return q }}
	crcUseSession(n.session)
	n.db.Count(nil)
	q.Find(nil)
}

// crcUseSession takes a session config.
func crcUseSession(*gorm.Session) {}

// crcAssignedBeforeBranch: every path assigns the captured variable before
// the returns, in another block, so the literal returns its own value.
func crcAssignedBeforeBranch(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		q = db.Session(&gorm.Session{})
		if c {
			return q
		}
		return q
	})
	q.Find(nil)
}

// crcMadeSliceKept: a local slice that make built and that is only built
// stays local, and the reuse of q is reported as before #397.
func crcMadeSliceKept(db *gorm.DB, n int) {
	q := db.Session(&gorm.Session{}).Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	fs := make([]func() *gorm.DB, n)
	fs[0] = func() *gorm.DB { return db }
	_ = fs
}

// crcNestedArray: a literal stored into an element of an element of a local
// array, only called.
func crcNestedArray(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	var a [1][1]func() *gorm.DB
	a[0][0] = func() *gorm.DB { return q }
	a[0][0]()
	q.Find(nil)
}

// crcCapturedFieldLater: the literal returns a field of a captured struct,
// read where it is handed on. A value stored into the field later is not
// what was handed on.
func crcCapturedFieldLater(db *gorm.DB) {
	h := crcHolder{db: db.Session(&gorm.Session{}).Where("x")}
	crcRun(func() *gorm.DB { return h.db })
	q2 := db.Session(&gorm.Session{}).Where("y")
	h.db = q2
	q2.Find(nil)
}

// ===== Local containers through aliases (#397, review round 7) =====
//
// go/ssa lowers make([]T, 1) and []T{...} to an Alloc and a Slice of it, and a
// variable assigned on a branch to a Phi. Each container below stays in the
// function, so the literal is not handed on.

// crcConstMake: a make with a constant length.
func crcConstMake(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := make([]func() *gorm.DB, 1)
	fs[0] = func() *gorm.DB { return q }
	for _, f := range fs {
		f()
	}
	q.Find(nil)
}

// crcConstMakeField: the same, into a field of an element.
func crcConstMakeField(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	js := make([]crcJob, 1)
	js[0].build = func() *gorm.DB { return q }
	crcUse(len(js) == 1)
	q.Find(nil)
}

// crcLitSliceIndexed: a slice literal, then a store into an element.
func crcLitSliceIndexed(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{nil}
	fs[0] = func() *gorm.DB { return q }
	for _, f := range fs {
		f()
	}
	q.Find(nil)
}

// crcPhiSlice: a slice variable assigned on a branch.
func crcPhiSlice(db *gorm.DB, n int, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := make([]func() *gorm.DB, n)
	if c {
		fs = make([]func() *gorm.DB, n+1)
	}
	fs[0] = func() *gorm.DB { return q }
	for _, f := range fs {
		f()
	}
	q.Find(nil)
}

// crcPhiMap: a map variable assigned on a branch.
func crcPhiMap(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	m := map[int]func() *gorm.DB{}
	if c {
		m = map[int]func() *gorm.DB{}
	}
	m[0] = func() *gorm.DB { return q }
	for _, f := range m {
		f()
	}
	q.Find(nil)
}

// crcAppendThenIndex: a store into an element of what append returned.
func crcAppendThenIndex(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	var fs []func() *gorm.DB
	fs = append(fs, nil)
	fs[0] = func() *gorm.DB { return q }
	for _, f := range fs {
		f()
	}
	q.Find(nil)
}

// crcCopyIntoContainer: the slice is stored into an element of another
// local slice.
func crcCopyIntoContainer(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := make([]func() *gorm.DB, 1)
	fs[0] = func() *gorm.DB { return q }
	outer := make([][]func() *gorm.DB, 1)
	outer[0] = fs
	for _, g := range outer {
		g[0]()
	}
	q.Find(nil)
}

// crcFnList is a named slice of builders.
type crcFnList []func() *gorm.DB

// crcNamedSlice: a conversion to a named slice type.
func crcNamedSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := crcFnList(make([]func() *gorm.DB, 1))
	fs[0] = func() *gorm.DB { return q }
	fs[0]()
	q.Find(nil)
}

// ===== The same containers, handed on =====

// crcRunFuncMap calls every builder of m.
func crcRunFuncMap(m map[int]func() *gorm.DB) {
	for _, f := range m {
		f().Find(nil)
	}
}

// crcMakeFns returns a slice of builders.
func crcMakeFns() []func() *gorm.DB {
	return make([]func() *gorm.DB, 1)
}

// ===== SHOULD REPORT (stores and containers) =====

// crcConditionalStore: the literal may assign another value to the captured
// variable. On the other path it returns q itself.
func crcConditionalStore(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		func() { crcUse(c) }()
		if c {
			q = db.Session(&gorm.Session{})
		}
		return q
	})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcLoadBeforeStore: the value is loaded before the variable is assigned, so
// the literal returns the value that orig holds too.
func crcLoadBeforeStore(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	orig := q
	crcRun(func() *gorm.DB {
		r := q
		q = db.Session(&gorm.Session{})
		return r
	})
	orig.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcReassignedBeforeClosure: another closure hands the literal on, after q
// became mutable. The variable is read where that closure is made.
func crcReassignedBeforeClosure(db *gorm.DB) {
	q := db.Session(&gorm.Session{})
	f := func() *gorm.DB { return q }
	q = q.Where("x")
	g := func() { crcRun(f) }
	g()
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcGetterChain: the literal handed on returns what another literal, held
// by a captured variable, returns.
func crcGetterChain(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return q }
	g := func() *gorm.DB { return f() }
	crcRun(g)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcConditionalStoreInClosure: a local closure, called directly, may assign
// the captured variable. When it does not, the literal returns q itself.
func crcConditionalStoreInClosure(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		reset := func() {
			if c {
				q = db.Session(&gorm.Session{})
			}
		}
		reset()
		return q
	})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcUse uses c.
func crcUse(c bool) {}

// crcGetterInLiteral: the literal calls a getter that it makes, through
// another literal it makes. The getter's capture is read where the outer
// literal is handed on.
func crcGetterInLiteral(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		f := func() *gorm.DB { return q }
		h := func() *gorm.DB { return f() }
		return h()
	})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcReadByOtherClosure: a closure handed to another function only reads the
// captured variable, so the literal still returns q itself.
func crcReadByOtherClosure(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		crcDo(func() { _ = q.Error })
		return q
	})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcNestedBranchesCaptured: a literal called in place inside the literal
// chains on q. That is the literal's one use of q, and q.Find is the second.
// The hand-off adds no branch, so the Count is not reported.
func crcNestedBranchesCaptured(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		func() { q.Count(nil) }()
		return q
	})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcRunAll calls every builder and chains on the results.
func crcRunAll(fs []func() *gorm.DB) {
	for _, f := range fs {
		f().Find(nil)
	}
}

// crcLocalSliceHandedOn: the local slice is passed to a call.
func crcLocalSliceHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	crcRunAll(fs)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcStoredFns holds builders.
var crcStoredFns []func() *gorm.DB

// crcLocalSliceToGlobal: the local slice is stored in a package-level
// variable.
func crcLocalSliceToGlobal(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	crcStoredFns = fs
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcRunMap calls every builder of every entry.
func crcRunMap(m map[string][]func() *gorm.DB) {
	for _, fs := range m {
		crcRunAll(fs)
	}
}

// crcRunV calls every builder it is passed and chains on the results.
func crcRunV(fs ...func() *gorm.DB) {
	crcRunAll(fs)
}

// crcVarargs: a literal handed on as a vararg is reported at the call.
func crcVarargs(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	q.Count(nil)
	crcRunV(func() *gorm.DB { return q }) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcVarargsInLoop: the hand-off runs again on each iteration, reported at
// the call.
func crcVarargsInLoop(db *gorm.DB, items []string) {
	q := db.Session(&gorm.Session{}).Where("x")
	for range items {
		crcRunV(func() *gorm.DB { return q }) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// crcReassignedBeforeCall: q is reassigned after the closure that hands the
// literal on is made, and before it is called. The variable is read at that
// call.
func crcReassignedBeforeCall(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return q }
	g := func() { crcRun(f) }
	q = db.Session(&gorm.Session{}).Where("y")
	g()
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcTakeAll takes values.
func crcTakeAll([]any) {}

// crcBoxedInSlice: a literal boxed in a []any that is passed on.
func crcBoxedInSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	vs := []any{func() *gorm.DB { return q }}
	crcTakeAll(vs)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcBuilder is a named func type.
type crcBuilder func() *gorm.DB

// crcRunBuilders calls every builder.
func crcRunBuilders(bs []crcBuilder) {
	for _, b := range bs {
		b().Find(nil)
	}
}

// crcNamedFuncInSlice: a literal converted to a named func type in a slice
// that is passed on.
func crcNamedFuncInSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	bs := []crcBuilder{func() *gorm.DB { return q }}
	crcRunBuilders(bs)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcRunArray calls every builder of an array.
func crcRunArray(fs [1]func() *gorm.DB) {
	fs[0]().Find(nil)
}

// crcLocalArrayHandedOn: a local array is passed on by value.
func crcLocalArrayHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := [1]func() *gorm.DB{func() *gorm.DB { return q }}
	crcRunArray(fs)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcLocalSliceInParamMap: the local slice is stored in a map the caller
// passed.
func crcLocalSliceInParamMap(db *gorm.DB, m map[string][]func() *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	m["q"] = fs
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcPtrS holds a builder, used through pointers.
type crcPtrS struct{ build func() *gorm.DB }

// crcGetterOfGetterInClosure: a closure hands on a literal that returns what
// a getter literal returns.
func crcGetterOfGetterInClosure(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	g := func() *gorm.DB { return q }
	f := func() *gorm.DB { return g() }
	func() { crcRun(f) }()
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcMethodValueGetterInClosure: a closure hands on a literal that calls a
// captured method value. The literal's call of q.Find is its use of q.
func crcMethodValueGetterInClosure(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	find := q.Find
	g := func() *gorm.DB { return find(nil) }
	func() { crcRun(g) }()
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcOtherPackageStruct: the literal is stored in a struct of another
// package that the function builds and passes on.
func crcOtherPackageStruct(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	cfg := hooklib.Config{Hook: func() *gorm.DB { return q }}
	hooklib.Run(cfg)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcOtherPackageStructDirect: the same, passed as it is built.
func crcOtherPackageStructDirect(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	hooklib.Run(hooklib.Config{Hook: func() *gorm.DB { return q }})
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcOtherPackageStructLocal: a struct of another package that is only built
// and read stays local.
func crcOtherPackageStructLocal(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	cfg := hooklib.Config{Hook: func() *gorm.DB { return q }}
	cfg.Hook()
	q.Find(nil)
}

// crcSharedGetter: two literals handed on call one getter literal, so each
// hand-off hands q on.
func crcSharedGetter(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return q }
	g1 := func() *gorm.DB { return f() }
	g2 := func() *gorm.DB { return f() }
	crcRun(g1)
	crcRun(g2) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcCapturedFieldTwice: the literal returns a field of a captured struct on
// two paths.
func crcCapturedFieldTwice(db *gorm.DB, c bool) {
	h := crcHolder{db: db.Session(&gorm.Session{}).Where("x")}
	crcRun(func() *gorm.DB {
		if c {
			return h.db
		}
		return h.db
	})
	h.db.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcMutualGetters: two getter literals call each other, and the second is
// handed on in a closure. Their answer is shared, so the closure hands q on.
func crcMutualGetters(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	var fa, fb func() *gorm.DB
	fa = func() *gorm.DB {
		if c {
			return fb()
		}
		return q
	}
	fb = func() *gorm.DB { return fa() }
	func() { crcUse(fa != nil) }()
	func() { crcRun(fb) }()
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcLiteralTwoValues: one literal is handed on as itself and through a
// variable that may hold it.
func crcLiteralTwoValues(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return q }
	g := func() *gorm.DB { return db.Session(&gorm.Session{}) }
	if c {
		g = f
	}
	crcRun(f)
	crcRun(g) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcSliceHolder holds builders.
type crcSliceHolder struct{ fs []func() *gorm.DB }

// crcParamFieldSlice: a literal stored into a slice that a parameter holds
// goes outside the function.
func crcParamFieldSlice(db *gorm.DB, p *crcSliceHolder) {
	q := db.Session(&gorm.Session{}).Where("x")
	p.fs[0] = func() *gorm.DB { return q }
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcSentSlice: a slice literal that holds the literal is sent.
func crcSentSlice(db *gorm.DB, ch chan []func() *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	ch <- []func() *gorm.DB{func() *gorm.DB { return q }}
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcReturnedSlice: a slice literal that holds the literal is returned.
func crcReturnedSlice(db *gorm.DB) []func() *gorm.DB {
	q := db.Session(&gorm.Session{}).Where("x")
	q.Find(nil)
	return []func() *gorm.DB{func() *gorm.DB { return q }} // want `\*gorm\.DB reused: second branch from mutable root`
}

// crcMethodValueInLocalSlice: a method value stored in a local slice is a
// hand-off, as before #397 (#374).
func crcMethodValueInLocalSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func(any, ...any) *gorm.DB{q.Find}
	_ = fs
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// ===== LIMITATIONS =====

// crcTake takes any value.
func crcTake(any) {}

// crcBoxedMethodValue: [LIMITATION] a method value boxed in an interface is
// not followed, so handing it on is no branch.
func crcBoxedMethodValue(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	var v any = q.Find
	crcTake(v)
	q.Count(nil)
}

// crcBranchesAndReturns: [LIMITATION] the literal chains on the captured
// value on one path and returns it unchanged on the other. Its chain is the
// literal's use of q, so the hand-off is not counted.
func crcBranchesAndReturns(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		if c {
			q = q.Where("z")
		}
		return q
	})
	q.Find(nil)
}

// crcDo calls f.
func crcDo(f func()) {
	f()
}

// crcAssignedByOtherClosure: [LIMITATION] a closure handed to another
// function may assign the captured variable, so when it runs is not known and
// the variable is left out.
func crcAssignedByOtherClosure(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		crcDo(func() {
			if c {
				q = db.Session(&gorm.Session{})
			}
		})
		return q
	})
	q.Find(nil)
}

// crcAssignedByOtherClosureNested: [LIMITATION] the same, with the closures
// nested in a literal called in place.
func crcAssignedByOtherClosureNested(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRun(func() *gorm.DB {
		func() {
			crcDo(func() {
				func() { q = db.Session(&gorm.Session{}) }()
			})
		}()
		return q
	})
	q.Find(nil)
}

// crcRunP calls fn and chains on what its result points to.
func crcRunP(fn func() **gorm.DB) {
	(*fn()).Find(nil)
}

// crcReturnsPointer: [LIMITATION] the literal returns the captured variable's
// address, not a *gorm.DB.
func crcReturnsPointer(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRunP(func() **gorm.DB { return &q })
	q.Find(nil)
}

// crcRunH calls fn and chains on the field of its result.
func crcRunH(fn func() crcHolder) {
	fn().db.Find(nil)
}

// crcReturnsStruct: [LIMITATION] the literal returns a struct that holds the
// captured value.
func crcReturnsStruct(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	crcRunH(func() crcHolder { return crcHolder{db: q} })
	q.Find(nil)
}

// crcFactoryLiteral: [LIMITATION] the literal handed on is made by a call of
// a local factory.
func crcFactoryLiteral(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	mk := func() func() *gorm.DB { return func() *gorm.DB { return q } }
	crcRun(mk())
	q.Find(nil)
}

// crcPureHandsOnLiteral: [LIMITATION] the pure check does not take a literal
// that returns the argument, handed on, as a branch of the argument. A
// method value handed on is one.
//
//gormreuse:pure
func crcPureHandsOnLiteral(d *gorm.DB) {
	crcRun(func() *gorm.DB { return d })
}

// crcGetterCalledInClosure: [LIMITATION] a closure that only calls a captured
// getter literal, and chains on the result, captures no *gorm.DB, so it is
// not analyzed.
func crcGetterCalledInClosure(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	f := func() *gorm.DB { return q }
	func() {
		f().Find(nil)
	}()
	q.Find(nil)
}

// ===== LIMITATIONS: literals in containers that are not handed on directly =====
//
// A literal stored into a container counts as handed on only when it goes
// straight into a container the function allocates, whose own value is then
// passed to a call, sent, stored outside the function or returned, or when it
// goes into a location outside the function. These containers are reached
// through a slice of an allocation, a merge, append, an element read, a
// capture, a pointer or another container, so the hand-off is not followed.

// crcConstMakeHandedOn: [LIMITATION] a make with a constant length, passed on.
func crcConstMakeHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := make([]func() *gorm.DB, 1)
	fs[0] = func() *gorm.DB { return q }
	crcRunAll(fs)
	q.Find(nil)
}

// crcConstMakeFieldHandedOn: [LIMITATION] the same, into a field of an element.
func crcConstMakeFieldHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	js := make([]crcJob, 1)
	js[0].build = func() *gorm.DB { return q }
	crcRunJob(js[0])
	q.Find(nil)
}

// crcLitSliceIndexedHandedOn: [LIMITATION] a slice literal, then a store into an
// element, passed on.
func crcLitSliceIndexedHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{nil}
	fs[0] = func() *gorm.DB { return q }
	crcRunAll(fs)
	q.Find(nil)
}

// crcPhiSliceHandedOn: [LIMITATION] a slice variable assigned on a branch, passed on.
func crcPhiSliceHandedOn(db *gorm.DB, n int, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := make([]func() *gorm.DB, n)
	if c {
		fs = make([]func() *gorm.DB, n+1)
	}
	fs[0] = func() *gorm.DB { return q }
	crcRunAll(fs)
	q.Find(nil)
}

// crcPhiMapHandedOn: [LIMITATION] a map variable assigned on a branch, passed on.
func crcPhiMapHandedOn(db *gorm.DB, c bool) {
	q := db.Session(&gorm.Session{}).Where("x")
	m := map[int]func() *gorm.DB{}
	if c {
		m = map[int]func() *gorm.DB{}
	}
	m[0] = func() *gorm.DB { return q }
	crcRunFuncMap(m)
	q.Find(nil)
}

// crcAppendThenIndexHandedOn: [LIMITATION] a store into an element of what append
// returned, passed on.
func crcAppendThenIndexHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	var fs []func() *gorm.DB
	fs = append(fs, nil)
	fs[0] = func() *gorm.DB { return q }
	crcRunAll(fs)
	q.Find(nil)
}

// crcCopyIntoContainerHandedOn: [LIMITATION] the slice is stored into an element of
// another local slice, whose element is passed on.
func crcCopyIntoContainerHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := make([]func() *gorm.DB, 1)
	fs[0] = func() *gorm.DB { return q }
	outer := make([][]func() *gorm.DB, 1)
	outer[0] = fs
	crcRunAll(outer[0])
	q.Find(nil)
}

// crcCapturedSliceIndexed: [LIMITATION] a closure captures the slice variable and passes
// it on.
func crcCapturedSliceIndexed(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := make([]func() *gorm.DB, 1)
	run := func() { crcRunAll(fs) }
	fs[0] = func() *gorm.DB { return q }
	run()
	q.Find(nil)
}

// crcCallResultSlice: [LIMITATION] a store into a slice that a call returned, which code
// outside the function may hold.
func crcCallResultSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := crcMakeFns()
	fs[0] = func() *gorm.DB { return q }
	q.Find(nil)
}

// crcAppendParam: [LIMITATION] a store into an element of what append returned from a
// slice the caller passed is a hand-off.
func crcAppendParam(db *gorm.DB, fs []func() *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs = append(fs, nil)
	fs[0] = func() *gorm.DB { return q }
	q.Find(nil)
}

// crcLocalSliceElemHandedOn: [LIMITATION] an element of the local slice is passed on.
func crcLocalSliceElemHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	crcRun(fs[0])
	q.Find(nil)
}

// crcLocalSliceResultUsed: [LIMITATION] a literal read from a local slice is called, and
// its result is chained on.
func crcLocalSliceResultUsed(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	fs[0]().Find(nil)
	q.Find(nil)
}

// crcLocalSliceCaptured: [LIMITATION] a closure captures the local slice, so it may leave
// the function.
func crcLocalSliceCaptured(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	g := func() { crcRunAll(fs) }
	g()
	q.Find(nil)
}

// crcLocalSliceInMap: [LIMITATION] the local slice is stored in a map that is passed on.
func crcLocalSliceInMap(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	crcRunMap(map[string][]func() *gorm.DB{"q": fs})
	q.Find(nil)
}

// crcLocalStructFieldHandedOn: [LIMITATION] a field of the local struct is passed on.
func crcLocalStructFieldHandedOn(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	j := crcJob{build: func() *gorm.DB { return q }}
	crcRun(j.build)
	q.Find(nil)
}

// crcLocalMapCommaOk: [LIMITATION] a value of the local map, read with comma-ok, is passed
// on.
func crcLocalMapCommaOk(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	m := map[string]func() *gorm.DB{"q": func() *gorm.DB { return q }}
	if f, ok := m["q"]; ok {
		crcRun(f)
	}
	q.Find(nil)
}

// crcLocalSliceElemSent: [LIMITATION] an element of the local slice is sent on a channel.
func crcLocalSliceElemSent(db *gorm.DB, ch chan func() *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	ch <- fs[0]
	q.Find(nil)
}

// crcAppended: [LIMITATION] a literal appended to a slice that is passed on is reported
// at the append.
func crcAppended(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	q.Count(nil)
	var fs []func() *gorm.DB
	fs = append(fs, func() *gorm.DB { return q })
	crcRunAll(fs)
}

// crcPointerElem: [LIMITATION] a field of a pointer element of a local slice is passed on.
func crcPointerElem(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	ps := []*crcPtrS{{build: func() *gorm.DB { return q }}}
	crcRun(ps[0].build)
	q.Find(nil)
}

// crcPointerElemRanged: [LIMITATION] the same, read in a range loop.
func crcPointerElemRanged(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	ps := []*crcPtrS{{build: func() *gorm.DB { return q }}}
	for _, p := range ps {
		crcRun(p.build)
	}
	q.Find(nil)
}

// crcPointerMapElem: [LIMITATION] a field of a pointer value of a local map is passed on.
func crcPointerMapElem(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	m := map[string]*crcPtrS{"q": {build: func() *gorm.DB { return q }}}
	crcRun(m["q"].build)
	q.Find(nil)
}

// crcPointerToVariable: [LIMITATION] a field read through a pointer to the variable that
// holds the struct is passed on.
func crcPointerToVariable(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	s := &crcPtrS{build: func() *gorm.DB { return q }}
	pp := &s
	crcRun((*pp).build)
	q.Find(nil)
}

// crcMapInSlice: [LIMITATION] a value of a map read out of a local slice is passed on.
func crcMapInSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	ms := []map[string]func() *gorm.DB{{"q": func() *gorm.DB { return q }}}
	crcRun(ms[0]["q"])
	q.Find(nil)
}

// crcElemBoxed: [LIMITATION] a function value read out of a local slice is boxed and
// passed on.
func crcElemBoxed(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := []func() *gorm.DB{func() *gorm.DB { return q }}
	var v any = fs[0]
	crcTake(v)
	q.Find(nil)
}

// crcOtherPackageStructInSlice: [LIMITATION] the same, as an element of a local slice.
func crcOtherPackageStructInSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	cs := []hooklib.Config{{Hook: func() *gorm.DB { return q }}}
	hooklib.Run(cs[0])
	q.Find(nil)
}

// crcArrayInSlice: [LIMITATION] an element of an array read out of a local slice is
// passed on.
func crcArrayInSlice(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	as := [][1]func() *gorm.DB{{func() *gorm.DB { return q }}}
	a := as[0]
	crcRun(a[0])
	q.Find(nil)
}

// crcMadeSlice: [LIMITATION] a literal stored into a slice that make built is passed on.
func crcMadeSlice(db *gorm.DB, n int) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := make([]func() *gorm.DB, n)
	fs[0] = func() *gorm.DB { return q }
	crcRun(fs[0])
	q.Find(nil)
}

// crcMadeSliceField: [LIMITATION] the same, into a field of an element.
func crcMadeSliceField(db *gorm.DB, n int) {
	q := db.Session(&gorm.Session{}).Where("x")
	js := make([]crcJob, n)
	js[0].build = func() *gorm.DB { return q }
	crcRun(js[0].build)
	q.Find(nil)
}

// crcMadeSliceOfSlices: [LIMITATION] the same, into an element of a slice element.
func crcMadeSliceOfSlices(db *gorm.DB, n int) {
	q := db.Session(&gorm.Session{}).Where("x")
	fss := make([][]func() *gorm.DB, n)
	fss[0] = make([]func() *gorm.DB, n)
	fss[0][0] = func() *gorm.DB { return q }
	crcRun(fss[0][0])
	q.Find(nil)
}

// crcMadeMap: [LIMITATION] a literal stored into a map that make built is passed on.
func crcMadeMap(db *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	m := make(map[string]func() *gorm.DB)
	m["q"] = func() *gorm.DB { return q }
	crcRun(m["q"])
	q.Find(nil)
}

// crcReceivedSlice: [LIMITATION] a literal stored into a slice received from
// a channel.
func crcReceivedSlice(db *gorm.DB, ch chan []func() *gorm.DB) {
	q := db.Session(&gorm.Session{}).Where("x")
	fs := <-ch
	fs[0] = func() *gorm.DB { return q }
	q.Find(nil)
}
