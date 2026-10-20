package internal

import "gorm.io/gorm"

// =============================================================================
// Methods that touch no clause (#307)
//
// AddError, DB, Get, InstanceGet and Callback read or set only state that no
// query is built from: db.Error, the connection pool, Statement.Settings and
// the callback manager. A call of one is not a use of its receiver at all.
// ToSQL builds SQL from a copy of the receiver's Statement, so it reads the
// receiver like Session does: an earlier branch changes what it reads.
// e2e/internal/state_methods_test.go checks each claim with GORM.
// =============================================================================

// ===== SHOULD NOT REPORT =====

// stateMethodsBeforeBranch: none of them is a first branch.
func stateMethodsBeforeBranch(db *gorm.DB, err error) {
	q := db.Where("x")
	q.AddError(err)
	_, _ = q.DB()
	_, _ = q.Get("k")
	_, _ = q.InstanceGet("k")
	_ = q.Callback()
	q.Find(nil)
}

// stateMethodsAfterBranch: none of them is a second branch, or a read of a
// changed root.
func stateMethodsAfterBranch(db *gorm.DB, err error) {
	q := db.Where("x")
	if e := q.Where("a").Find(nil).Error; e != nil {
		q.AddError(e)
	}
	q.AddError(err)
	_, _ = q.Get("k")
	_, _ = q.InstanceGet("k")
	_ = q.Callback()
	_, _ = q.DB()
}

// stateMethodsSQLDBThenMigrate: the issue's user repro on a parameter.
func stateMethodsSQLDBThenMigrate(db *gorm.DB) {
	sqlDB, _ := db.DB()
	_ = sqlDB
	_ = db.AutoMigrate(nil)
}

// stateMethodsMethodValue: a call through a method value is the method call.
func stateMethodsMethodValue(db *gorm.DB, err error) {
	q := db.Where("x")
	addError := q.AddError
	addError(err)
	q.Find(nil)
}

// stateMethodsToSQLThenFind: ToSQL reads q without branching it.
func stateMethodsToSQLThenFind(db *gorm.DB) string {
	q := db.Where("x")
	s := q.ToSQL(func(tx *gorm.DB) *gorm.DB { return tx.Find(nil) })
	q.Find(nil)
	return s
}

// stateMethodsPure: a pure function may call them on its argument.
//
//gormreuse:pure
func stateMethodsPure(db *gorm.DB, err error) error {
	_, _ = db.Get("k")
	_ = db.ToSQL(func(tx *gorm.DB) *gorm.DB { return tx.Find(nil) })
	return db.AddError(err)
}

// ===== SHOULD REPORT =====

// stateMethodsToSQLAfterBranch: ToSQL reads the condition the branch added.
func stateMethodsToSQLAfterBranch(db *gorm.DB) string {
	q := db.Where("x")
	q.Where("a").Find(nil)
	return q.ToSQL(func(tx *gorm.DB) *gorm.DB { return tx.Find(nil) }) // want `\*gorm\.DB reused: second branch from mutable root`
}

// stateMethodsStillBranch: the others still count. The second Find reuses q.
func stateMethodsStillBranch(db *gorm.DB, err error) {
	q := db.Where("x")
	q.AddError(err)
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// stateMethodsMigratorBranches: Migrator runs the receiver's pending scopes
// in place and keeps a copy of it, so it stays a branch.
func stateMethodsMigratorBranches(db *gorm.DB) {
	q := db.Where("x")
	_ = q.Migrator()
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
