package internal

import "gorm.io/gorm"

// =============================================================================
// SHOULD REPORT - Derived variables without Session
// =============================================================================

// derivedVariables demonstrates unsafe derived variables.
func derivedVariables(db *gorm.DB) {
	queryDB := db.Model(&User{}).Where("name = ?", "jinzhu").Session(&gorm.Session{})
	q := queryDB.Where("age > ?", 10) // Derived without Session - mutable
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB instance reused after chain method`
}

// storedChainResultReuse demonstrates reuse of stored chain result.
func storedChainResultReuse(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	q.Where("name = ?", "x").Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// storedChainResultMultipleDerivations demonstrates multiple derivations from same base.
func storedChainResultMultipleDerivations(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1)
	base.Where("type = ?", "A").Find(nil)
	base.Where("type = ?", "B").Find(nil) // want `\*gorm\.DB instance reused after chain method`
}

// derivedFromSessionUnsafe demonstrates that chain after Session is still mutable.
func derivedFromSessionUnsafe(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1)
	safe := base.Session(&gorm.Session{})
	derived := safe.Where("active = ?", true) // Mutable!

	derived.Find(nil)
	derived.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// multiLevelUnsafeDerivation demonstrates chained derivation.
func multiLevelUnsafeDerivation(db *gorm.DB) {
	level1 := db.Where("a")
	level2 := level1.Where("b")
	level3 := level2.Where("c")

	level3.Find(nil)
	level3.Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Derived with Session
// =============================================================================

// derivedWithSession demonstrates safe derived variables with Session.
func derivedWithSession(db *gorm.DB) {
	queryDB := db.Model(&User{}).Where("name = ?", "jinzhu").Session(&gorm.Session{})
	q := queryDB.Where("age > ?", 10).Session(&gorm.Session{})
	q.Find(&[]User{})
	q.Count(new(int64)) // OK: Session at end
}

// sessionResetsClone demonstrates Session creating safe copy.
func sessionResetsClone(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1)
	safe := base.Session(&gorm.Session{})

	safe.Where("type = ?", "A").Find(nil)
	safe.Where("type = ?", "B").Find(nil) // OK: independent chains from safe
}

// withContextResetsClone demonstrates WithContext creating safe copy.
func withContextResetsClone(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1)
	safe := base.WithContext(nil)

	safe.Where("type = ?", "A").Find(nil)
	safe.Where("type = ?", "B").Find(nil) // OK: independent chains from safe
}

// sessionAtEachDerivation demonstrates Session at each derivation point.
func sessionAtEachDerivation(db *gorm.DB) {
	base := db.Where("tenant_id = ?", 1).Session(&gorm.Session{})
	derived := base.Where("active = ?", true).Session(&gorm.Session{})

	derived.Find(nil)
	derived.Count(nil) // OK: ends with Session
}

// =============================================================================
// SHOULD REPORT - Function return value
// =============================================================================

func helperWhere(db *gorm.DB, name string) *gorm.DB {
	return db.Model(&User{}).Where("name = ?", name)
}

// functionReturnValue demonstrates unsafe reuse of function return.
func functionReturnValue(db *gorm.DB) {
	q := helperWhere(db, "jinzhu")
	q.Find(&[]User{})
	q.Count(new(int64)) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Function return with Session
// =============================================================================

// functionReturnWithSession demonstrates safe function return with Session.
func functionReturnWithSession(db *gorm.DB) {
	q := helperWhere(db, "jinzhu").Session(&gorm.Session{})
	q.Find(&[]User{})
	q.Count(new(int64)) // OK: Session at end
}

// =============================================================================
// SHOULD REPORT - Session on polluted value
// =============================================================================

// sessionAfterPolluted demonstrates that Session() after pollution doesn't help.
func sessionAfterPolluted(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true)
	q.Count(new(int64))                        // q is polluted
	q.Session(&gorm.Session{}).Find(&[]User{}) // want `\*gorm\.DB instance reused after chain method`
}

// sessionOnPollutedValue demonstrates Session on already-polluted value.
func sessionOnPollutedValue(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)
	q.Session(&gorm.Session{}).Count(nil) // want `\*gorm\.DB instance reused after chain method`
}

// multipleDirectUsesWithoutSession demonstrates multiple uses without Session.
func multipleDirectUsesWithoutSession(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)
	q.Count(nil)                          // want `\*gorm\.DB instance reused after chain method`
	q.Session(&gorm.Session{}).First(nil) // want `\*gorm\.DB instance reused after chain method`
}

// =============================================================================
// SHOULD NOT REPORT - Session before each finisher
// =============================================================================

// sessionBeforeEachFinisher demonstrates Session before each finisher.
func sessionBeforeEachFinisher(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true)

	q.Session(&gorm.Session{}).Count(new(int64))
	q.Session(&gorm.Session{}).Find(&[]User{}) // OK: Session before each use
}

// sessionBeforeFinisher demonstrates Session before each finisher (variant).
func sessionBeforeFinisher(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Session(&gorm.Session{}).Find(nil)
	q.Session(&gorm.Session{}).Count(nil) // OK: Session before each finisher
}

// =============================================================================
// SHOULD NOT REPORT - Reassignment
// =============================================================================

// reassignNewInstance demonstrates that reassigning a variable is safe.
func reassignNewInstance(db *gorm.DB) {
	q := db.Model(&User{}).Where("active = ?", true)
	q.Count(new(int64))

	q = db.Where("name = ?", "test") // New instance assigned
	q.Find(&[]User{})                // OK
}
