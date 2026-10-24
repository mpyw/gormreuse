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
// PURE FUNCTIONS THAT WRITE THE PARAMETER'S QUERY THROUGH A DIRECT PATH (#405)
//
// A write into the parameter's Statement, or over the *gorm.DB itself,
// changes its query in place, as db.Where does. So it breaks the pure
// contract, and callers stop trusting the function. Only a direct path from
// the parameter counts: its fields, and the maps and slices its Statement
// holds, also in a closure, a go or a defer statement in the body.
// =============================================================================

//gormreuse:pure
func pswDistinct(db *gorm.DB) {
	db.Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswClauses(db *gorm.DB) {
	db.Statement.Clauses["X"] = 1 // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswSelects(db *gorm.DB) {
	db.Statement.Selects = []string{"id"} // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	db.Statement.Selects[0] = "id"        // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswRegister names the Statement in a local that go/ssa keeps in a
// register: it is the same value, so the path is direct.
//
//gormreuse:pure
func pswRegister(db *gorm.DB) {
	st := db.Statement
	st.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswOverwrite(db *gorm.DB) {
	*db = gorm.DB{} // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswReplaceStatement(db *gorm.DB) {
	db.Statement = nil // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswBuiltins(db *gorm.DB) {
	delete(db.Statement.Clauses, "WHERE")      // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	clear(db.Statement.Clauses)                // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	clear(db.Statement.Selects)                // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	copy(db.Statement.Selects, []string{"id"}) // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswStatementParam(db *gorm.DB, st *gorm.Statement) {
	st.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

//gormreuse:pure
func pswStatementDB(db *gorm.DB) {
	db.Statement.DB.Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswCaptured writes through the captured parameter in a closure, a nested
// closure, a go and a defer statement, and a deferred builtin.
//
//gormreuse:pure
func pswCaptured(db *gorm.DB) {
	func() {
		db.Statement.Distinct = true // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	}()
	func() {
		func() { db.Statement.Selects = nil }() // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	}()
	go func() { delete(db.Statement.Clauses, "X") }() // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	defer func() { db.Statement.Table = "" }()        // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
	defer delete(db.Statement.Clauses, "Y")           // want `pure function pollutes \*gorm\.DB argument by writing to it directly`
}

// pswSelfStore and pswOverwriteFrom store the argument into memory it
// reaches. Each is reported once, as the escape.
//
//gormreuse:pure
func pswSelfStore(db *gorm.DB) {
	db.Statement.DB = db // want `pure function leaks \*gorm\.DB argument via store through a pointer`
}

//gormreuse:pure
func pswOverwriteFrom(a, b *gorm.DB) {
	*a = *b // want `pure function leaks \*gorm\.DB argument via store through a pointer`
}

// pswCallerSubquery: pswDistinct(sub) changes the subquery that outer reads,
// as it would without the directive.
func pswCallerSubquery(db *gorm.DB) {
	sub := db.Session(&gorm.Session{}).Model(nil).Select("id")
	outer := db.Session(&gorm.Session{}).Where("id IN (?)", sub)
	pswDistinct(sub)
	outer.Find(nil) // want `\*gorm\.DB reused: mutable root passed as a query argument`
}

// pswCallerSecondBranch: pswOverwrite is no longer trusted, so passing q to
// it is a branch.
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
func pswRead(db *gorm.DB) bool {
	return db.Statement.Distinct && len(db.Statement.Selects) > 0
}

// pswReaders reads the Statement's maps and slices, also through functions,
// and appends to a copy.
//
//gormreuse:pure
func pswReaders(db *gorm.DB) string {
	_ = slices.Contains(db.Statement.Selects, "id")
	fmt.Println(db.Statement.Clauses, db.Statement.Table)
	_ = append([]string{}, db.Statement.Selects...)
	dst := make([]string, 1)
	copy(dst, db.Statement.Selects)
	_ = db.Statement.Clauses["WHERE"]
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

// pswKeyedByHandle uses the argument as a map key. The map is the package's.
//
//gormreuse:pure
func pswKeyedByHandle(db *gorm.DB) {
	pswPerDB[db]["n"]++
}

var pswPerDB = map[*gorm.DB]map[string]int{}

// pswLocalCopy writes the fields of local copies, and a new Statement.
//
//gormreuse:pure
func pswLocalCopy(db *gorm.DB) bool {
	c := *db.Statement
	c.Distinct = true
	d := *db
	d.Statement = &gorm.Statement{}
	d.Statement.Distinct = true
	st := &gorm.Statement{DB: db, Clauses: map[string]interface{}{}}
	st.Clauses["X"] = 1
	return c.Distinct
}

// pswHolderWrites writes holder parameters themselves, not a query.
//
//gormreuse:pure
func pswHolderWrites(pq **gorm.DB, h *pswHolder, m map[string]*gorm.DB) {
	*pq = nil
	h.db = nil
	m["a"] = nil
}

type pswHolder struct{ db *gorm.DB }

var pswMk func(*gorm.DB) *gorm.DB

// pswDynHandle writes the Statement of a handle that a function value
// returned. It may not be the argument, so only the warning for passing the
// argument to a function that is not pure stays.
//
//gormreuse:pure
func pswDynHandle(db *gorm.DB) {
	q := pswMk(db) // want `pure function passes \*gorm\.DB argument to non-pure function \(closure\)`
	q.Statement.Distinct = true
}

func pswCallerTrusted(db *gorm.DB) {
	q := db.Where("x")
	pswRead(q)
	pswOutcome(q)
	pswReaders(q)
	pswLocalCopy(q)
	q.Find(nil)
}

// =============================================================================
// [LIMITATION] WRITES THROUGH ANYTHING BUT A DIRECT PATH
//
// A write through a local variable that is assigned again, captured or has
// its address taken, a copy, a Session or Debug result, a holder parameter,
// an append result, a lookup, a conversion, a merge, or a call is not
// followed. Each may write memory that the argument does not hold.
// =============================================================================

// pswNotDirect writes the argument's query through paths that are not
// direct.
// [LIMITATION]
//
//gormreuse:pure
func pswNotDirect(db *gorm.DB, other *gorm.DB, c bool, b []byte) {
	st := db.Statement
	func() { st.Distinct = true }() // a captured variable
	cp := *db.Statement
	cp.Clauses["X"] = 1 // a copy holds the same map
	h := *db
	h.Statement.Distinct = true                           // a copy holds the same Statement
	db.Session(&gorm.Session{}).Statement.Distinct = true // a Session that shares it
	db.Debug().Statement.Selects = nil                    // so does Debug
	_ = append(db.Statement.Selects[:0], "x")             // an append overwrites it
	db.Statement.Preloads["A"][0] = 1                     // a lookup
	(*pswStatement)(db.Statement).Distinct = true         // a conversion
	pswFill(db.Statement.Clauses)                         // a call
	sort.Strings(db.Statement.Selects)                    // a call
	_ = json.Unmarshal(b, &db.Statement.Selects)          // a call gets the address
	db.Statement.SQL.WriteString("x")                     // a method of a field
	m := other.Statement
	if c {
		m = db.Statement
	}
	m.Distinct = true // a merge
}

type pswStatement gorm.Statement

func pswFill(m map[string]interface{}) { m["X"] = 1 }

// pswHolderParams writes the query of a handle that a holder parameter holds.
// [LIMITATION]
//
//gormreuse:pure
func pswHolderParams(h pswHolder, pq **gorm.DB, arr [1]*gorm.DB, m map[string]*gorm.DB, st gorm.Statement) {
	h.db.Statement.Distinct = true
	(*pq).Statement.Distinct = true
	arr[0].Statement.Distinct = true
	m["a"].Statement.Distinct = true
	st.Clauses["X"] = 1
}

// pswReassignedParam writes through a parameter that the body assigns again,
// in a closure.
// [LIMITATION]
//
//gormreuse:pure
func pswReassignedParam(db *gorm.DB) {
	func() { db.Statement.Distinct = true }()
	db = nil
}

// pswConfig writes the argument's Config. It holds no clause, so it is
// allowed, but a write such as DryRun or Logger changes how the caller's
// later queries run.
// [LIMITATION]
//
//gormreuse:pure
func pswConfig(db *gorm.DB) {
	db.Config.SkipDefaultTransaction = true
	db.SkipDefaultTransaction = false
}

func pswCallerNotDirect(db *gorm.DB) {
	q := db.Where("x")
	pswNotDirect(q, nil, false, nil) // missed: pswNotDirect is still trusted
	q.Find(nil)
}

// =============================================================================
// INFERRED IMMUTABLE-PARAM (#405)
//
// A helper that writes its parameter's query through a direct path relies on
// changing the caller's value in place, so it is not inferred, and its reuse
// is reported. A helper that only reads the Statement is inferred.
// =============================================================================

func pswInferClauses(db *gorm.DB) {
	db.Statement.Clauses["X"] = 1
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferSelectsElem(db *gorm.DB) {
	db.Statement.Selects[0] = "id"
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferBuiltins(db *gorm.DB) {
	delete(db.Statement.Clauses, "WHERE")
	copy(db.Statement.Selects, []string{"id"})
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferCaptured(db *gorm.DB) {
	func() { clear(db.Statement.Clauses) }()
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferStatementDB(db *gorm.DB) {
	db.Statement.DB.Statement.Clauses["X"] = 1
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// The helpers below only read the Statement, or write memory it does not
// hold. They are inferred, as on main, and their reuse is not reported.
func pswInferReads(db *gorm.DB) {
	_ = db.Statement.Clauses["WHERE"]
	_ = db.Statement.Selects[0]
	pswProbeLog(db.Statement.Table)
	fmt.Println("t", db.Statement.Table, db.Statement.Selects)
	_ = db.Statement.Context.Value("k")
	_ = strings.Join(db.Statement.Selects, ",")
	db.Find(nil)
	db.Count(nil)
}

func pswInferStores(db *gorm.DB, out *pswOut, m map[string]string) {
	out.Table = db.Statement.Table
	m["t"] = db.Statement.Table
	db.Find(nil)
	db.Count(nil)
}

func pswInferCopies(db *gorm.DB) {
	s := db.Statement.Selects
	s = append(s[:0:0], s...)
	s[0] = "x"
	dst := make([]string, 1)
	copy(dst, db.Statement.Selects)
	db.Find(nil)
	db.Count(nil)
}

// pswInferNotDirect writes the parameter's query through paths that are not
// direct, so it is still inferred.
// [LIMITATION]
func pswInferNotDirect(db *gorm.DB) {
	c := db.Statement.Clauses
	func() { c["X"] = 1 }()
	pswFill(db.Statement.Clauses)
	sort.Strings(db.Statement.Selects)
	db.Session(&gorm.Session{}).Statement.Distinct = true
	_ = append(db.Statement.Selects[:0], "x")
	db.Find(nil)
	db.Count(nil)
}

func pswInferCallers(db *gorm.DB) {
	pswInferClauses(db.Session(&gorm.Session{}))
	pswInferSelectsElem(db.Session(&gorm.Session{}))
	pswInferBuiltins(db.Session(&gorm.Session{}))
	pswInferCaptured(db.Session(&gorm.Session{}))
	pswInferStatementDB(db.Session(&gorm.Session{}))
	pswInferReads(db.Session(&gorm.Session{}))
	pswInferStores(db.Session(&gorm.Session{}), nil, nil)
	pswInferCopies(db.Session(&gorm.Session{}))
	pswInferNotDirect(db.Session(&gorm.Session{}))
}
