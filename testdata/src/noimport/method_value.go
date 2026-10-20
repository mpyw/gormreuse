// Package noimport tests fix generation when gorm is not directly imported.
package noimport

import internal "gormreuse"

// methodValueWithoutGormImport calls a method value of a *gorm.DB twice in a
// package that does not import gorm (#374). The SSA package of gorm is not
// created then, so the method value's wrapper has no declared method in the
// program. It is still a gorm method call.
func methodValueWithoutGormImport() {
	q := internal.GetQuery()
	w := q.Where
	w("a").Find(nil)
	w("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// methodValueLeakWithoutGormImport reassigns q, then branches the old q
// through a method value taken before (#374). The method value's receiver is
// still the first q, so the call through it is a second branch of it.
func methodValueLeakWithoutGormImport() {
	q := internal.GetQuery()
	where := q.Where
	q.Where("a")
	where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	q.Find(nil)          // want `\*gorm\.DB reused: second branch from mutable root`
}
