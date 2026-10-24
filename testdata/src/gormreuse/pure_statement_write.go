package internal

import "gorm.io/gorm"

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

//gormreuse:pure
func pswFreshHandle(db *gorm.DB) {
	x := db.Session(&gorm.Session{})
	x.Statement.Distinct = true
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

func pswInferHandedOn(db *gorm.DB) {
	pswFill(db.Statement.Clauses)
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswFill(m map[string]interface{}) { m["X"] = 1 }

func pswInferSent(db *gorm.DB, ch chan map[string]interface{}) {
	ch <- db.Statement.Clauses
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func pswInferStoredInMap(db *gorm.DB, m map[string]map[string]interface{}) {
	m["c"] = db.Statement.Clauses
	db.Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
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
}
