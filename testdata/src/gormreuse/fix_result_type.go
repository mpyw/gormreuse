package internal

import "gorm.io/gorm"

// Fixtures for issue #200: the reassignment fix rewrites only a statement whose
// call returns *gorm.DB and does not run a query. `q = q.AddError(err)` assigns
// an error to q, and `q = q.Commit()` rebinds q to a finished transaction.

// fixResultTypeAddError: AddError returns an error, so it is not reassigned.
// It touches no clause either, so it is not a branch (#307).
func fixResultTypeAddError(db *gorm.DB, err error) {
	q := db.Where("x")
	q.AddError(err)
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixResultTypeAutoMigrate: AutoMigrate returns an error.
func fixResultTypeAutoMigrate(db *gorm.DB) {
	q := db.Where("x")
	q.AutoMigrate(nil)
	q.Where("a").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixResultTypeDB: DB returns two values. It touches no clause, so it is not
// a branch (#307).
func fixResultTypeDB(db *gorm.DB) {
	q := db.Where("x")
	q.DB()
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixResultTypeAssociation: Association returns a *gorm.Association.
func fixResultTypeAssociation(db *gorm.DB) {
	q := db.Where("x")
	q.Association("Users")
	q.Where("a").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixResultTypeMigrator: Migrator returns a gorm.Migrator.
func fixResultTypeMigrator(db *gorm.DB) {
	q := db.Where("x")
	q.Migrator()
	q.Where("a").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixResultTypeCommit: Commit ends the transaction, so it is a finisher.
func fixResultTypeCommit(db *gorm.DB) {
	q := db.Begin().Where("x")
	q.Where("a").Find(nil)
	q.Commit() // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixResultTypeRollback: Rollback ends the transaction, so it is a finisher.
func fixResultTypeRollback(db *gorm.DB) {
	q := db.Begin().Where("x")
	q.Where("a").Find(nil)
	q.Rollback() // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixResultTypeFindInBatches: FindInBatches runs queries, so it is a finisher.
func fixResultTypeFindInBatches(db *gorm.DB) {
	q := db.Where("x")
	q.FindInBatches(nil, 10, func(tx *gorm.DB, batch int) error { return nil })
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// fixResultTypeInBatchesAndColumns: CreateInBatches and UpdateColumn(s) run
// queries too.
func fixResultTypeInBatchesAndColumns(db *gorm.DB) {
	q := db.Where("x")
	q.CreateInBatches(nil, 10)
	q.UpdateColumn("a", 1) // want `\*gorm\.DB reused: second branch from mutable root`
	q.UpdateColumns(nil)   // want `\*gorm\.DB reused: second branch from mutable root`
}
