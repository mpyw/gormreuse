package internal

import "gorm.io/gorm"

// Issue #311: fixes that needed a second -fix pass and left the code buggy at
// runtime after the first. TestFixesConverge re-lints the golden.

type laterLoopFilter struct{ Name string }

// laterLoopReassigned: the reassignment q = q.Where("tenant") is read on each
// iteration of a later loop, so it needs a Session, or each query adds its
// id to the ones before.
func laterLoopReassigned(db *gorm.DB, ids []int) {
	q := db.Where("x")
	q.Where("tenant")
	for _, id := range ids {
		q.Where("id = ?", id).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// laterLoopReassignedInIf: the reassignment in the if is read in the loop,
// and so is the original root when c is false. Both get a Session.
func laterLoopReassignedInIf(db *gorm.DB, ids []int, c bool) {
	q := db.Where("x")
	if c {
		q.Where("t")
	}
	for _, id := range ids {
		q.Where("id = ?", id).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// laterLoopNested: the read is in an inner loop.
func laterLoopNested(db *gorm.DB, rows [][]int) {
	q := db.Where("x")
	q.Where("tenant")
	for _, row := range rows {
		for _, id := range row {
			q.Where("id = ?", id).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
		}
	}
}

// laterLoopReassignedInLoop: a reassignment in one loop is read in a later
// one.
func laterLoopReassignedInLoop(db *gorm.DB, names []string, ids []int) {
	q := db.Where("x")
	for _, name := range names {
		q.Where("name <> ?", name) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	for _, id := range ids {
		q.Where("id = ?", id).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// laterLoopParam: the reassignment of a parameter is read in a later loop.
// The function returns q, which carries the change back to the caller.
func laterLoopParam(q *gorm.DB, ids []int) *gorm.DB {
	q.Where("tenant")
	for _, id := range ids {
		q.Where("id = ?", id).Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	return q
}

func laterLoopCallsParam(db *gorm.DB) {
	laterLoopParam(db.Where("scoped"), nil).Find(nil)
}

// laterLoopPaginate is standard pagination: the Where in the if merges with
// the parameter, and the merge is read twice. The parameter gets a Session at
// the top of the body, since a caller may pass a mid-chain value, and the
// Where gets its own, or Count's Model and Select leak into Find.
func laterLoopPaginate(db *gorm.DB, f laterLoopFilter) (int64, error) {
	var total int64
	if f.Name != "" {
		db = db.Where("name = ?", f.Name)
	}
	if err := db.Count(&total).Error; err != nil {
		return 0, err
	}
	err := db.Offset(10).Limit(10).Find(nil).Error // want `\*gorm\.DB reused: second branch from mutable root`
	return total, err
}

func laterLoopCallsPaginate(db *gorm.DB) {
	_, _ = laterLoopPaginate(db.Where("scoped"), laterLoopFilter{})
}

// laterLoopPaginateIsolated is laterLoopPaginate with only isolated callers.
// The parameter is declared immutable-param, and the Where gets a Session.
func laterLoopPaginateIsolated(db *gorm.DB, f laterLoopFilter) (int64, error) {
	var total int64
	if f.Name != "" {
		db = db.Where("name = ?", f.Name)
	}
	if err := db.Count(&total).Error; err != nil {
		return 0, err
	}
	err := db.Offset(10).Limit(10).Find(nil).Error // want `\*gorm\.DB reused: second branch from mutable root`
	return total, err
}

func laterLoopCallsPaginateIsolated(db *gorm.DB) {
	_, _ = laterLoopPaginateIsolated(db.Session(&gorm.Session{}), laterLoopFilter{})
}
