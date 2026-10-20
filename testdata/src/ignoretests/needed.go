package ignoretests

import "gorm.io/gorm"

// ignoreTestsRepo is the case of #348. Only the test variant sees the store in
// needed_test.go, so only it finds the reuse that the ignore covers. The
// variant without the test files must not report the ignore unused.
type ignoreTestsRepo struct{ db *gorm.DB }

func (r *ignoreTestsRepo) DB() *gorm.DB { return r.db }

func ignoreTestsUse(r *ignoreTestsRepo) {
	d := r.DB()
	d.Find(nil)
	//gormreuse:ignore
	d.Count(nil)
}
