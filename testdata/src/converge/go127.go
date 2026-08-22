package converge

import "gorm.io/gorm"

// go127Row is a local model.
type go127Row struct{ ID uint }

// go127Repo carries the generic methods (Go 1.27 allows a method to declare its
// own type parameters).
type go127Repo struct{}

// genericMethodReuse: two branches from a mutable root inside a generic method;
// fix = Session on the root.
func (r go127Repo) genericMethodReuse[T any](db *gorm.DB, out *T) {
	q := db.Where("x")
	q.Find(out)
	q.Count(new(int64)) // want `\*gorm\.DB reused: second branch from mutable root`
}

type go127Embedded struct{ db *gorm.DB }

type go127Holder struct {
	go127Embedded
	n int
}

// promotedFieldReuse: the root is reached through a promoted field key (Go 1.27
// allows filling an embedded field by its promoted name); fix = Session on the
// root.
func promotedFieldReuse(db *gorm.DB) {
	q := db.Where("x")
	h := go127Holder{db: q, n: 1}
	h.db.Find(&go127Row{})
	h.db.Count(new(int64)) // want `\*gorm\.DB reused: second branch from mutable root`
}
