package linedirective

import "gorm.io/gorm"

// Every function in this file sits below a //line directive that renames the
// file (issue #151).

//line ignore_line.tmpl:1
func ignoreLineBelowLineDirective(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	//gormreuse:ignore
	q.Count(nil)
}

// Control: the same reuse without the ignore is still reported.
func reportBelowLineDirective(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root \(root at ignore_line\.tmpl:10, first branch at ignore_line\.tmpl:11\)`
}
