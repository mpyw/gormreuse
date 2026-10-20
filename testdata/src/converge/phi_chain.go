package converge

import "gorm.io/gorm"

// Pagination with several conditional filters, from issue #343. Each filter
// adds a Phi that takes the one before, and Count and Find read the last. One
// pass must put a Session on the root and on every filter: a Session on the
// first filter alone made the next filter's Where a new mutable root, so
// -fix needed one pass per filter.

// phiDB is a package-level handle. A function that only reads it gets an
// immutable value, so the root is its Model call.
var phiDB *gorm.DB

type phiFilter struct{ A, B, C, D, E string }

func phiLocal2(db *gorm.DB, f phiFilter) {
	var n int64
	q := db.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func phiLocal3(db *gorm.DB, f phiFilter) {
	var n int64
	q := db.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	if f.C != "" {
		q = q.Where("c = ?", f.C)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func phiLocal5(db *gorm.DB, f phiFilter) {
	var n int64
	q := db.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	if f.C != "" {
		q = q.Where("c = ?", f.C)
	}
	if f.D != "" {
		q = q.Where("d = ?", f.D)
	}
	if f.E != "" {
		q = q.Where("e = ?", f.E)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func phiParam2(db *gorm.DB, f phiFilter) {
	var n int64
	if f.A != "" {
		db = db.Where("a = ?", f.A)
	}
	if f.B != "" {
		db = db.Where("b = ?", f.B)
	}
	db.Model(&User{}).Count(&n)
	db.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func phiParam3(db *gorm.DB, f phiFilter) {
	var n int64
	if f.A != "" {
		db = db.Where("a = ?", f.A)
	}
	if f.B != "" {
		db = db.Where("b = ?", f.B)
	}
	if f.C != "" {
		db = db.Where("c = ?", f.C)
	}
	db.Model(&User{}).Count(&n)
	db.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func phiParam5(db *gorm.DB, f phiFilter) {
	var n int64
	if f.A != "" {
		db = db.Where("a = ?", f.A)
	}
	if f.B != "" {
		db = db.Where("b = ?", f.B)
	}
	if f.C != "" {
		db = db.Where("c = ?", f.C)
	}
	if f.D != "" {
		db = db.Where("d = ?", f.D)
	}
	if f.E != "" {
		db = db.Where("e = ?", f.E)
	}
	db.Model(&User{}).Count(&n)
	db.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func phiGlobal2(f phiFilter) {
	var n int64
	q := phiDB.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func phiGlobal3(f phiFilter) {
	var n int64
	q := phiDB.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	if f.C != "" {
		q = q.Where("c = ?", f.C)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

func phiGlobal5(f phiFilter) {
	var n int64
	q := phiDB.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	if f.C != "" {
		q = q.Where("c = ?", f.C)
	}
	if f.D != "" {
		q = q.Where("d = ?", f.D)
	}
	if f.E != "" {
		q = q.Where("e = ?", f.E)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phiElse merges both branches of an if with else before the next filter.
func phiElse(db *gorm.DB, f phiFilter) {
	var n int64
	q := db.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	} else {
		q = q.Where("a IS NULL")
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phiLoopBetween has a loop between two filters. The diagnostic's root is the
// last filter, so the Phis before it are followed back through the
// loop-header Phi.
func phiLoopBetween(db *gorm.DB, f phiFilter, ids []int) {
	var n int64
	q := db.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	for _, id := range ids {
		q = q.Where("id <> ?", id)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phiLoopFirst has the loop before the filters.
func phiLoopFirst(db *gorm.DB, f phiFilter, ids []int) {
	var n int64
	q := db.Model(&User{})
	for _, id := range ids {
		q = q.Where("id <> ?", id)
	}
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phiLoopFilter has a conditional filter inside the loop.
func phiLoopFilter(db *gorm.DB, f phiFilter, ids []int) {
	var n int64
	q := db.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	for _, id := range ids {
		if id > 0 {
			q = q.Where("id <> ?", id)
		}
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phiParamLoop is a parameter with a loop between two filters.
func phiParamLoop(db *gorm.DB, f phiFilter, ids []int) {
	var n int64
	if f.A != "" {
		db = db.Where("a = ?", f.A)
	}
	for _, id := range ids {
		db = db.Where("id <> ?", id)
	}
	if f.B != "" {
		db = db.Where("b = ?", f.B)
	}
	db.Model(&User{}).Count(&n)
	db.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phiGlobalLoop is a root on a package-level handle with a loop between two
// filters.
func phiGlobalLoop(f phiFilter, ids []int) {
	var n int64
	q := phiDB.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	for _, id := range ids {
		q = q.Where("id <> ?", id)
	}
	if f.B != "" {
		q = q.Where("b = ?", f.B)
	}
	q.Count(&n)
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}

// phiDiscarded has a discarded filter between two conditional ones. It is
// reassigned, so it keeps its condition under the Sessions.
func phiDiscarded(db *gorm.DB, f phiFilter) {
	var n int64
	q := db.Model(&User{})
	if f.A != "" {
		q = q.Where("a = ?", f.A)
	}
	q.Where("deleted_at IS NULL")
	if f.B != "" {
		q = q.Where("b = ?", f.B) // want `\*gorm\.DB reused: second branch from mutable root`
	}
	q.Count(&n) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Limit(10).Find(&[]User{}) // want `\*gorm\.DB reused: second branch from mutable root`
}
