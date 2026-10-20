package converge

import "gorm.io/gorm"

// escapeGoFinisher: a go statement runs a query on q, which hands q to no
// other code. The Session at q resolves every diagnostic (#312).
func escapeGoFinisher(db *gorm.DB) {
	q := db.Model(nil)
	q.Find(nil)
	go q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.First(nil)    // want `\*gorm\.DB reused: second branch from mutable root`
}

// escapeTenantUsed changes tx in place, and every caller reads its result,
// so the reassignment keeps the change (#312).
func escapeTenantUsed(tx *gorm.DB) (*gorm.DB, error) {
	tx.Where("a = 1").Find(nil)
	tx.Where("tenant_id = 7") // want `\*gorm\.DB reused: second branch from mutable root`
	return tx, nil
}

func escapeCallsTenantUsed(db *gorm.DB) error {
	q, err := escapeTenantUsed(db.Model(nil))
	q.Find(nil)
	return err
}

// escapeIgnoredOther: q's fix leaves the ignore on r's reuse used (#312).
func escapeIgnoredOther(db, other *gorm.DB) {
	q := db.Model(nil)
	r := other.Model(nil)
	q.Find(nil)
	r.Find(nil)
	r.Count(nil) //gormreuse:ignore
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
