package internal

import "gorm.io/gorm"

// =============================================================================
// CLOSURE IN LOOP - User's original pattern
// =============================================================================

type bannerRepo struct {
	db *gorm.DB
}

func (r *bannerRepo) DB() *gorm.DB {
	return r.db.Session(&gorm.Session{})
}

// whereStatusFixed: User's pattern - closure in loop with switch
// Each closure call should be independent, no false positives.
func (r *bannerRepo) whereStatusFixed(statuses []int) (*gorm.DB, error) {
	publishedQueryFunc := func(isPublished bool) (*gorm.DB, error) {
		if isPublished {
			return r.DB().Where("published = ?", true), nil
		}
		return r.DB().Where("published = ?", false), nil
	}

	condition := r.DB()

	for _, status := range statuses {
		switch status {
		case 1:
			publishedQuery, err := publishedQueryFunc(false)
			if err != nil {
				return nil, err
			}
			// Each publishedQuery is independent - should NOT report
			condition = condition.
				Or(publishedQuery.
					Where("x").
					Or("y"))
		case 2:
			publishedQuery, err := publishedQueryFunc(true)
			if err != nil {
				return nil, err
			}
			condition = condition.
				Or(publishedQuery.
					Where("a").
					Where("b"))
		case 3:
			condition = condition.Or("z")
		}
	}

	return condition, nil
}

// =============================================================================
// CONDITIONAL RETURN - Multiple returns in closure
// =============================================================================

// iifeConditionalReturnSameRoot: IIFE with conditional returns from same root
func iifeConditionalReturnSameRoot(db *gorm.DB) {
	q := db.Where("base")

	// Both returns derive from same root (q)
	_ = func() *gorm.DB {
		if true {
			return q.Where("a")
		}
		return q.Where("b")
	}().Find(nil)

	q.Count(nil) // want "\\*gorm\\.DB instance reused"
}

// iifeConditionalReturnDifferentRoots: IIFE with conditional returns from different roots
func iifeConditionalReturnDifferentRoots(db *gorm.DB) {
	q1 := db.Where("q1")
	q2 := db.Where("q2")

	q1.Find(nil) // Pollute q1

	// Returns from different roots - should detect if any root is polluted
	_ = func() *gorm.DB {
		if true {
			return q1.Where("a") // want "\\*gorm\\.DB instance reused"
		}
		return q2.Where("b") // q2 is clean
	}().Find(nil)
}

// storedClosureSingleReturn: Stored closure with single return value
func storedClosureSingleReturn(db *gorm.DB) {
	q := db.Where("base")

	getQuery := func() *gorm.DB {
		return q.Where("inner")
	}

	// Single return - stored closure result should be independent
	result := getQuery()
	result.Find(nil)

	q.Count(nil) // want "\\*gorm\\.DB instance reused"
}

// storedClosureMultiReturn: Stored closure with multi return value
func storedClosureMultiReturn(db *gorm.DB) {
	q := db.Where("base")

	getQuery := func() (*gorm.DB, error) {
		return q.Where("inner"), nil
	}

	// Multi return with Extract - stored closure result should be independent
	result, _ := getQuery()
	result.Find(nil)

	q.Count(nil) // want "\\*gorm\\.DB instance reused"
}

// =============================================================================
// FIXED: Single-return closure + method chain in loop
// =============================================================================

// loopWithSingleReturnClosure: Loop with single-return closure
// Correctly handles single-return closures by detecting the chain flows to MakeInterface
// (via condition.Or's variadic interface{} argument), treating each call as independent root.
func loopWithSingleReturnClosure(db *gorm.DB, items []int) {
	getQuery := func() *gorm.DB {
		return db.Session(&gorm.Session{}).Where("fresh")
	}

	condition := db.Session(&gorm.Session{})

	for range items {
		q := getQuery()
		// Each getQuery() call is treated as independent root
		// because q.Where("x") flows to MakeInterface for condition.Or()
		condition = condition.Or(q.Where("x"))
	}

	condition.Find(nil)
}

// loopWithMultiReturnClosure: Loop with multi-return closure
// Multi-return goes through Extract, so isClosureResultStored returns true,
// treating each call as independent root.
func loopWithMultiReturnClosure(db *gorm.DB, items []int) {
	getQuery := func() (*gorm.DB, error) {
		return db.Session(&gorm.Session{}).Where("fresh"), nil
	}

	condition := db.Session(&gorm.Session{})

	for range items {
		q, _ := getQuery()
		// Each getQuery() call is independent root (via Extract)
		condition = condition.Or(q.Where("x"))
	}

	condition.Find(nil)
}
