package before

import "gorm.io/gorm"

// handoffFindByID only runs a query on tx, so the only change it makes to
// the caller's value is the pollution of its query (#347).
func handoffFindByID(tx *gorm.DB, id int) error {
	return tx.Where("id = ?", id).Find(&[]User{}).Error
}

// FinisherOnlyHelper hands q to a helper that only runs a query on it, then
// queries q again. The helper's condition leaks into the second query. The
// fix puts a Session at q.
func FinisherOnlyHelper(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	_ = handoffFindByID(q, 1)
	q.Where("name = ?", "x").Find(&[]User{})
}

// handoffWalk queries tx and calls itself with it. Each level's condition
// leaks into the levels below. The fix rebinds tx to a Session at the top of
// the body, so each call changes only its own copy.
func handoffWalk(tx *gorm.DB, depth int) {
	tx.Where("depth = ?", depth).Find(&[]User{})
	if depth < 2 {
		handoffWalk(tx, depth+1)
	}
}

// RecursiveHelper runs handoffWalk on a filtered value.
func RecursiveHelper(db *gorm.DB) {
	handoffWalk(db.Model(&User{}).Where("tenant_id = ?", 7), 0)
}
