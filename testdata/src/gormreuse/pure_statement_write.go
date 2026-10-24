package internal

import "gorm.io/gorm"

// =============================================================================
// PURE FUNCTIONS THAT WRITE TO THE ARGUMENT DIRECTLY (#405)
//
// A write into the argument's Statement, or over the *gorm.DB itself, changes
// its query in place, as db.Where does. So it breaks the pure contract, and
// callers stop trusting the function. The query's outcome (Error,
// RowsAffected) and a local copy of the Statement may be written.
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
}
