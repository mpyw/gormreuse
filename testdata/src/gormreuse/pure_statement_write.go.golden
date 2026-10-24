package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// =============================================================================
// PURE FUNCTIONS THAT WRITE TO THE ARGUMENT DIRECTLY (#405)
//
// A write into the argument's Statement, or over the *gorm.DB itself, changes
// its query in place, as db.Where does. So it breaks the pure contract, and
// callers stop trusting the function. The query's outcome (Error,
// RowsAffected) and the scalar fields of a local copy of the Statement may be
// written. The maps and slices of such a copy are still the argument's.
// =============================================================================

type pswHolder struct{ db *gorm.DB }

//gormreuse:pure
func pswDistinct(db *gorm.DB) {
	db.Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswClauses(db *gorm.DB) {
	db.Statement.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswViaLocal(db *gorm.DB) {
	st := db.Statement
	st.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswSelects(db *gorm.DB) {
	db.Statement.Selects = append(db.Statement.Selects, "id") // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswOverwrite(db *gorm.DB) {
	*db = gorm.DB{} // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswViaHolder(h pswHolder) {
	h.db.Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswViaPointer(pq **gorm.DB) {
	(*pq).Statement.Table = "t" // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswInClosure(db *gorm.DB) {
	func() {
		db.Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	}()
}

//gormreuse:pure
func pswCopiedHandle(db *gorm.DB) {
	c := *db
	c.Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswCapturedStatement(db *gorm.DB) {
	st := db.Statement
	func() {
		st.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	}()
}

//gormreuse:pure
func pswCapturedStatementVar(db *gorm.DB) {
	st := db.Statement
	func() {
		st.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	}()
	st = nil
	_ = st
}

//gormreuse:pure
func pswLocalCopyMap(db *gorm.DB) {
	c := *db.Statement
	c.Distinct = true
	c.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswLocalCopySlice(db *gorm.DB) {
	c := *db.Statement
	c.Distinct = true
	c.Selects[0] = "id" // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswStatementParam(db *gorm.DB, st *gorm.Statement) {
	st.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswDelete(db *gorm.DB) {
	delete(db.Statement.Clauses, "WHERE") // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswClear(db *gorm.DB) {
	clear(db.Statement.Clauses) // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswCopyInto(db *gorm.DB) {
	copy(db.Statement.Selects, []string{"id"}) // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswMerged(db *gorm.DB, other *gorm.DB, c bool) {
	st := other.Statement
	if c {
		st = db.Statement
	}
	st.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswLocalCopyValue(db *gorm.DB) {
	c := *db.Statement
	c.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswArrayParam(arr [1]*gorm.DB) {
	arr[0].Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswMapParam(m map[string]*gorm.DB) {
	m["a"].Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

type pswStatement gorm.Statement

//gormreuse:pure
func pswConverted(db *gorm.DB) {
	st := (*pswStatement)(db.Statement)
	st.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswLoopSlice(db *gorm.DB, n int) {
	s := db.Statement.Selects
	for i := 0; i < n; i++ {
		s = s[1:]
	}
	s[0] = "id" // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswLoopMerged(db *gorm.DB, n int) {
	s := []string{"a", "b"}
	for i := 0; i < n; i++ {
		if i == 1 {
			s = db.Statement.Selects
		}
		s = s[1:]
	}
	s[0] = "id" // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswSelfStore: storing the argument into its own memory is reported once, as
// the escape.
//
//gormreuse:pure
func pswSelfStore(db *gorm.DB) {
	db.Statement.DB = db // want `pure function leaks \*gorm\.DB argument via store through a pointer`
}

//gormreuse:pure
func pswOverwriteFrom(a, b *gorm.DB) {
	*a = *b // want `pure function leaks \*gorm\.DB argument via store through a pointer`
}

// pswSessionShared writes the Statement of a Session or Debug copy, which
// shares the argument's Statement (#405 review).
//
//gormreuse:pure
func pswSessionShared(db *gorm.DB) {
	x := db.Session(&gorm.Session{})
	x.Statement.Distinct = true                                  // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	db.Session(&gorm.Session{}).Statement.Clauses["X"] = 1       // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	db.Debug().Statement.Selects = nil                           // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	db.Session(&gorm.Session{DryRun: true}).Statement.Table = "" // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswAppendResliced appends to a slice of the Statement's Selects, which
// overwrites its elements in place.
//
//gormreuse:pure
func pswAppendResliced(db *gorm.DB) {
	_ = append(db.Statement.Selects[:0], "x") // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswAppendShorter appends to a reslice of Selects cut at another slice's
// length, which may be below its own.
//
//gormreuse:pure
func pswAppendShorter(db *gorm.DB, other []string) {
	_ = append(db.Statement.Selects[:len(other)], "x") // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswConvertedSlice writes through a slice converted to an array pointer, and
// through a map boxed in an interface and asserted back.
//
//gormreuse:pure
func pswConvertedSlice(db *gorm.DB) {
	(*[1]string)(db.Statement.Selects)[0] = "x" // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	var m any = db.Statement.Clauses
	m.(map[string]interface{})["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswHandsOnMemory hands the Statement's map and slice to functions that
// change them. A hand-off to a call is not followed, as it was not before
// #405: the callee's body would decide it.
// [LIMITATION]
//
//gormreuse:pure
func pswHandsOnMemory(db *gorm.DB) {
	pswFill(db.Statement.Clauses)
	sort.Strings(db.Statement.Selects)
}

// pswCapturedLocalCopy writes the shared map of a local copy in a closure.
//
//gormreuse:pure
func pswCapturedLocalCopy(db *gorm.DB) {
	c := *db.Statement
	func() {
		c.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	}()
}

// pswReplacedLater replaces the copy's map only after the write, so the write
// reaches the argument.
//
//gormreuse:pure
func pswReplacedLater(db *gorm.DB, cond bool) {
	c := *db.Statement
	if cond {
		c.Clauses = map[string]interface{}{}
	}
	c.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswSessionNewDB writes the Statement of a NewDB Session, which gorm does
// not clone either. The Session config reader takes NewDB as isolating,
// which holds for a chain on the copy, not for a write to its Statement.
// [LIMITATION]
//
//gormreuse:pure
func pswSessionNewDB(db *gorm.DB) {
	db.Session(&gorm.Session{NewDB: true}).Statement.Distinct = true
}

// pswBoxedHandedOn hands the Statement's map, boxed in an interface, to a
// function that asserts it back and writes it. A boxed value is followed only
// into a type assertion in the function.
// [LIMITATION]
//
//gormreuse:pure
func pswBoxedHandedOn(db *gorm.DB) {
	pswFillAny(db.Statement.Clauses)
}

func pswFillAny(v any) { v.(map[string]interface{})["X"] = 1 }

// pswPreloads writes the slices that the Statement's Preloads map holds:
// through a lookup, a comma-ok lookup and a range over the map.
//
//gormreuse:pure
func pswPreloads(db *gorm.DB) {
	db.Statement.Preloads["A"][0] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	if v, ok := db.Statement.Preloads["B"]; ok {
		v[0] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	}
	for _, v := range db.Statement.Preloads {
		v[0] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	}
}

// pswBoxedMerged asserts back a map boxed in an interface that a merge may
// hold.
//
//gormreuse:pure
func pswBoxedMerged(db *gorm.DB, other any, c bool) {
	var m any = db.Statement.Clauses
	if c {
		m = other
	}
	m.(map[string]interface{})["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswStoredInClosure stores the map into a variable inside a closure, and
// writes it after.
//
//gormreuse:pure
func pswStoredInClosure(db *gorm.DB) {
	var c map[string]interface{}
	func() { c = db.Statement.Clauses }()
	c["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswReplacedInClosure replaces the copy's map in a closure, which may not
// run before the write, and pswReplacedAfter only after the write.
//
//gormreuse:pure
func pswReplacedInClosure(db *gorm.DB) {
	c := *db.Statement
	func() { c.Clauses = map[string]interface{}{} }()
	c.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswReplacedAfter(db *gorm.DB) {
	c := *db.Statement
	c.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	c.Clauses = nil
}

// pswLocalArray stores the map into an element of a local array at an index
// that is not a constant, so a read of any element may be it.
//
//gormreuse:pure
func pswLocalArrayAnyIndex(db *gorm.DB, i int) {
	var arr [2]map[string]interface{}
	arr[i] = db.Statement.Clauses
	arr[0]["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswStatementValue writes the map of a gorm.Statement passed by value: the
// map is still the caller's.
//
//gormreuse:pure
func pswStatementValue(db *gorm.DB, st gorm.Statement) {
	st.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswLiteralCall hands the map to a function literal, which changes it.
// [LIMITATION]
//
//gormreuse:pure
func pswLiteralCall(db *gorm.DB) {
	func(m map[string]interface{}) { m["X"] = 1 }(db.Statement.Clauses)
}

// pswCopyByAddress hands the address of a local copy to a function, which
// reaches the maps the copy shares.
//
//gormreuse:pure
func pswCopyByAddress(db *gorm.DB) {
	c := *db.Statement
	pswTakeStatement(&c) // want `pure function passes \*gorm\.DB argument to non-pure function pswTakeStatement`
}

func pswTakeStatement(st *gorm.Statement) { st.Clauses["X"] = 1 }

// pswAddressHandedOn hands the address of a Statement field to functions
// that write through it, and calls methods of the Statement's SQL builder.
// [LIMITATION]
//
//gormreuse:pure
func pswAddressHandedOn(db *gorm.DB, b []byte) {
	_ = json.Unmarshal(b, &db.Statement.Selects)
	_, _ = fmt.Sscan("t", &db.Statement.Table)
	db.Statement.SQL.WriteString("DROP")
	db.Statement.SQL.Reset()
}

// pswKeyedByHandle uses the argument as a map key. The map is the package's,
// not the argument's memory.
//
//gormreuse:pure
func pswKeyedByHandle(db *gorm.DB) {
	pswPerDB[db]["n"]++
	if m, ok := pswPerDB[db]; ok {
		m["n"] = 0
	}
}

var pswPerDB = map[*gorm.DB]map[string]int{}

func pswCallerHandsOn(db *gorm.DB) {
	q := db.Where("x")
	pswHandsOnMemory(q) // missed: pswHandsOnMemory is still trusted
	q.Find(nil)
}

// pswCallerSubquery: mark(sub) changes the subquery that outer reads, as
// it does without the directive.
func pswCallerSubquery(db *gorm.DB) {
	sub := db.Session(&gorm.Session{}).Model(nil).Select("id")
	outer := db.Session(&gorm.Session{}).Where("id IN (?)", sub)
	pswDistinct(sub)
	outer.Find(nil) // want `\*gorm\.DB reused: mutable root passed as a query argument`
}

// pswCallerSecondBranch: the call is a branch again, so the later query is the
// second one.
func pswCallerSecondBranch(db *gorm.DB) {
	q := db.Where("x")
	pswOverwrite(q)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// =============================================================================
// SHOULD NOT REPORT
// =============================================================================

//gormreuse:pure
func pswOutcome(db *gorm.DB) {
	db.Error = nil
	db.RowsAffected = 0
}

//gormreuse:pure
func pswLocalCopy(db *gorm.DB) bool {
	c := *db.Statement
	c.Distinct = true
	return c.Distinct
}

//gormreuse:pure
func pswCopiedHandleOutcome(db *gorm.DB) error {
	c := *db
	c.Error = nil
	return c.Error
}

type pswHolderPtr struct {
	db *gorm.DB
	n  int
}

//gormreuse:pure
func pswHolderScalar(h *pswHolderPtr) {
	h.n = 1
}

//gormreuse:pure
func pswCopyFrom(db *gorm.DB) []string {
	dst := make([]string, 1)
	copy(dst, db.Statement.Selects)
	return dst
}

//gormreuse:pure
func pswLocalArray(db *gorm.DB) {
	var arr [1]*gorm.DB
	arr[0] = nil
	s := arr[:]
	s[0] = nil
	_ = db
}

//gormreuse:pure
func pswCapturedReassigned(db *gorm.DB) {
	st := db.Statement
	func() {
		st = nil
	}()
	_ = st
}

// pswRebindPointer changes the caller's variable, not the *gorm.DB it holds.
//
//gormreuse:pure
func pswRebindPointer(pq **gorm.DB) {
	*pq = nil
}

// pswSessionCloned writes the Statement of a Session that clones it: one
// with Context, SkipHooks or Initialized, or WithContext.
//
//gormreuse:pure
func pswSessionCloned(db *gorm.DB, ctx context.Context) {
	db.Session(&gorm.Session{Context: ctx}).Statement.Distinct = true
	db.Session(&gorm.Session{SkipHooks: true}).Statement.Distinct = true
	db.Session(&gorm.Session{Initialized: true}).Statement.Distinct = true
	db.WithContext(ctx).Statement.Distinct = true
}

// pswFreshStatement writes a map that the function made, held by a new
// Statement or by a local copy whose field it replaced first (#405 review).
//
//gormreuse:pure
func pswFreshStatement(db *gorm.DB) {
	st := &gorm.Statement{DB: db, Clauses: map[string]interface{}{}}
	st.Clauses["X"] = 1
	c := *db.Statement
	c.Clauses = map[string]interface{}{}
	c.Clauses["X"] = 1
}

// pswReaders hands the Statement's slice and map to functions that only read
// them, and boxes them for fmt.
//
//gormreuse:pure
func pswReaders(db *gorm.DB) string {
	_ = slices.Contains(db.Statement.Selects, "id")
	_ = append([]string{}, db.Statement.Selects...)
	s := db.Statement.Selects
	_ = append(s[:len(s):len(s)], "x")
	_ = append(db.Statement.Selects[:0:0], "x")
	_ = append(db.Statement.Selects[1:], "x")
	_ = append(db.Statement.Selects[:len(db.Statement.Selects)], "x")
	fmt.Println(db.Statement.Clauses, db.Statement.Table)
	return strings.Join(db.Statement.Selects, ",")
}

// pswScalars hands on and stores scalar and interface fields of the
// Statement: copies, which writes do not change.
//
//gormreuse:pure
func pswScalars(db *gorm.DB, out *pswOut, m map[string]string, ch chan context.Context) {
	pswProbeLog(db.Statement.Table)
	out.Table = db.Statement.Table
	m["t"] = db.Statement.Table
	ch <- db.Statement.Context
	_ = db.Statement.Context.Value("k")
}

func pswProbeLog(string) {}

type pswOut struct{ Table string }

// pswConfig writes the argument's Config. It holds no clause, so it is
// allowed, but a write such as DryRun or Logger changes how the caller's
// later queries run. gorm's Session copies the Config, so the caller's handle
// may hold the same one.
// [LIMITATION]
//
//gormreuse:pure
func pswConfig(db *gorm.DB) {
	db.Config.SkipDefaultTransaction = true
	db.SkipDefaultTransaction = false
}

//gormreuse:pure
func pswRead(db *gorm.DB) bool {
	return db.Statement.Distinct && len(db.Statement.Selects) > 0
}

// pswCallerTrusted: a pure function that only reads stays trusted.
func pswCallerTrusted(db *gorm.DB) {
	q := db.Where("x")
	pswRead(q)
	pswOutcome(q)
	q.Find(nil)
}

// =============================================================================
// INFERRED IMMUTABLE-PARAM (#405)
//
// A helper that writes to its parameter's Statement relies on changing the
// caller's value in place, so it is not inferred immutable-param. Its reuse
// stays reported, also when the write goes through a map or a slice that the
// Statement holds.
// =============================================================================

func pswInferClauses(db *gorm.DB) {
	db.Statement.Clauses["X"] = db.Statement.Clauses["WHERE"]
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferSelectsElem(db *gorm.DB) {
	db.Statement.Selects[0] = "id"
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferDistinct(db *gorm.DB) {
	db.Statement.Distinct = true
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferDelete(db *gorm.DB) {
	delete(db.Statement.Clauses, "WHERE")
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferCopy(db *gorm.DB) {
	copy(db.Statement.Selects, []string{"id"})
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferSlice(db *gorm.DB) {
	s := db.Statement.Selects[1:]
	s[0] = "id"
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferCaptured(db *gorm.DB) {
	c := db.Statement.Clauses
	func() { c["X"] = 1 }()
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferCapturedVar(db *gorm.DB) {
	c := db.Statement.Clauses
	func() { c["X"] = 1 }()
	c = nil
	_ = c
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferAppendShared(db *gorm.DB) {
	s := append(db.Statement.Selects[:0], "id")
	s[0] = "name"
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// pswInferHandedOn hands the Statement memory on, which is not followed, as before
// #405.
// [LIMITATION]
func pswInferHandedOn(db *gorm.DB) {
	pswFill(db.Statement.Clauses)
	db.Find(nil)
	db.Count(nil)
}

func pswFill(m map[string]interface{}) { m["X"] = 1 }

// pswInferSent hands the Statement memory on, which is not followed, as before
// #405.
// [LIMITATION]
func pswInferSent(db *gorm.DB, ch chan map[string]interface{}) {
	ch <- db.Statement.Clauses
	db.Find(nil)
	db.Count(nil)
}

// pswInferStoredInMap hands the Statement memory on, which is not followed, as before
// #405.
// [LIMITATION]
func pswInferStoredInMap(db *gorm.DB, m map[string]map[string]interface{}) {
	m["c"] = db.Statement.Clauses
	db.Find(nil)
	db.Count(nil)
}

// pswInferReadsInClosure and pswInferReadsInLoop only read the Statement, in a
// closure and through a slice merged in a loop. They are inferred.
func pswInferReadsInClosure(db *gorm.DB) {
	c := db.Statement.Clauses
	func() { _ = c["X"] }()
	db.Find(nil)
	db.Count(nil)
}

func pswInferReadsInLoop(db *gorm.DB) {
	for s := db.Statement.Selects; len(s) > 0; s = s[1:] {
		_ = s[0]
	}
	db.Find(nil)
	db.Count(nil)
}

// pswInferReadsCopy reads the Statement only: copy reads its source, and
// append's result is not written through. It is inferred.
func pswInferReadsCopy(db *gorm.DB) {
	dst := make([]string, 1)
	copy(dst, db.Statement.Selects)
	_ = append(db.Statement.Selects, "id")
	_ = len(db.Statement.Clauses)
	db.Find(nil)
	db.Count(nil)
}

// pswInferReads only reads the Statement, so it is inferred, and its reuse is
// not reported.
func pswInferReads(db *gorm.DB) {
	_ = db.Statement.Clauses["WHERE"]
	_ = db.Statement.Selects[0]
	db.Find(nil)
	db.Count(nil)
}

// pswInferSorted hands the Statement memory on, which is not followed, as before
// #405.
// [LIMITATION]
func pswInferSorted(db *gorm.DB) {
	sort.Strings(db.Statement.Selects)
	db.Find(nil)
	db.Count(nil)
}

func pswInferAppendResliced(db *gorm.DB) {
	_ = append(db.Statement.Selects[:0], "x")
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferConverted(db *gorm.DB) {
	(*[1]string)(db.Statement.Selects)[0] = "x"
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferBoxed(db *gorm.DB) {
	var m any = db.Statement.Clauses
	m.(map[string]interface{})["X"] = 1
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// The helpers below only read scalar and interface fields of the Statement,
// or hand its slice to a reader. Their writes change copies, so they are
// inferred as on main, and their reuse is not reported (#405 review).
func pswInferLogsTable(db *gorm.DB) {
	pswProbeLog(db.Statement.Table)
	db.Find(nil)
	db.Count(nil)
}

func pswInferPrintsTable(db *gorm.DB) {
	fmt.Println("t", db.Statement.Table)
	db.Find(nil)
	db.Count(nil)
}

func pswInferStoresTable(db *gorm.DB, out *pswOut) {
	out.Table = db.Statement.Table
	db.Find(nil)
	db.Count(nil)
}

func pswInferMapsTable(db *gorm.DB, m map[string]string) {
	m["t"] = db.Statement.Table
	db.Find(nil)
	db.Count(nil)
}

func pswInferContext(db *gorm.DB) {
	ctx := db.Statement.Context
	_ = ctx.Value("k")
	db.Find(nil)
	db.Count(nil)
}

func pswInferJoins(db *gorm.DB) {
	_ = strings.Join(db.Statement.Selects, ",")
	_ = slices.Index(db.Statement.Selects, "id")
	db.Find(nil)
	db.Count(nil)
}

// pswInferSessionShared writes the Statement of a Session copy of its
// parameter, which is the parameter's. The inference reads the fields of the
// parameter itself only.
// [LIMITATION]
func pswInferSessionShared(db *gorm.DB) {
	db.Session(&gorm.Session{}).Statement.Distinct = true
	db.Find(nil)
	db.Count(nil)
}

func pswInferCallers(db *gorm.DB) {
	pswInferClauses(db.Session(&gorm.Session{}))
	pswInferSelectsElem(db.Session(&gorm.Session{}))
	pswInferDistinct(db.Session(&gorm.Session{}))
	pswInferReads(db.Session(&gorm.Session{}))
	pswInferDelete(db.Session(&gorm.Session{}))
	pswInferCopy(db.Session(&gorm.Session{}))
	pswInferSlice(db.Session(&gorm.Session{}))
	pswInferCaptured(db.Session(&gorm.Session{}))
	pswInferCapturedVar(db.Session(&gorm.Session{}))
	pswInferAppendShared(db.Session(&gorm.Session{}))
	pswInferHandedOn(db.Session(&gorm.Session{}))
	pswInferSent(db.Session(&gorm.Session{}), nil)
	pswInferStoredInMap(db.Session(&gorm.Session{}), nil)
	pswInferReadsCopy(db.Session(&gorm.Session{}))
	pswInferReadsInClosure(db.Session(&gorm.Session{}))
	pswInferReadsInLoop(db.Session(&gorm.Session{}))
	pswInferSorted(db.Session(&gorm.Session{}))
	pswInferAppendResliced(db.Session(&gorm.Session{}))
	pswInferConverted(db.Session(&gorm.Session{}))
	pswInferBoxed(db.Session(&gorm.Session{}))
	pswInferLogsTable(db.Session(&gorm.Session{}))
	pswInferPrintsTable(db.Session(&gorm.Session{}))
	pswInferStoresTable(db.Session(&gorm.Session{}), nil)
	pswInferMapsTable(db.Session(&gorm.Session{}), nil)
	pswInferContext(db.Session(&gorm.Session{}))
	pswInferJoins(db.Session(&gorm.Session{}))
	pswInferSessionShared(db.Session(&gorm.Session{}))
}
