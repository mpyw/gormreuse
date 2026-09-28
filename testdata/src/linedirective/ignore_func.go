package linedirective

import "gorm.io/gorm"

// A function-level ignore below a //line directive (issue #151).

//line ignore_func.tmpl:1
//gormreuse:ignore
func ignoreFuncBelowLineDirective(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	q.Count(nil)
}
