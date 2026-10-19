package converge

import "gorm.io/gorm"

// discardedMultiCall: a discarded statement with several calls (#199). The fix
// reassigns the whole statement and puts its Session after the last call. A
// Session after q.Where(...) alone would be undone by .Limit(10), which
// returns a fresh mutable value, so re-linting would still warn.
func discardedMultiCall(db *gorm.DB) {
	q := db.Model(nil).Where("tenant_id = ?", 7)
	q.Where("deleted_at IS NULL").Limit(10)
	q.Where("x").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Where("y").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// discardedLoop: a discarded statement after the loop's query (#199). The loop
// violation is reported while the query is tracked, before the discarded
// statement is, but the fix still reassigns it. Its Session keeps the next
// iteration's query from changing it.
func discardedLoop(db *gorm.DB, xs []int) {
	q := db.Model(nil).Where("tenant_id = ?", 7)
	for _, x := range xs {
		q.Where("id > ?", x).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		q.Where("age > ?", x)          // want `\*gorm\.DB reused: second branch from mutable root`
	}
}
