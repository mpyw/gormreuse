// Package aliasimport tests that suggested fixes compile when gorm is imported
// under an alias: the inserted Session must use the local name (g.Session),
// not a hardcoded gorm.Session (issue #71, defect 3).
package aliasimport

import g "gorm.io/gorm"

func aliasedReuse(db *g.DB) {
	q := db.Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
