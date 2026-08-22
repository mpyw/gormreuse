package internal

import "gorm.io/gorm"

// =============================================================================
// ALIAS-TYPED *gorm.DB
//
// go/types always materializes an alias declaration (`type Q = *gorm.DB`) as a
// *types.Alias node — the gotypesalias GODEBUG that used to turn this off is
// gone. Every predicate that inspects a DECLARED type (parameters, results,
// struct fields, receivers) therefore has to see through the alias, otherwise
// alias-typed signatures would silently drop out of tracking and their
// directives would be reported as unused.
// =============================================================================

type aliasDB = gorm.DB
type aliasQuery = *gorm.DB

// =============================================================================
// SHOULD REPORT
// =============================================================================

type aliasHolder struct{ db aliasQuery }

// aliasFieldReuse: the reused value is stored in an alias-typed struct field.
func aliasFieldReuse(db *gorm.DB) {
	q := db.Where("x")
	h := aliasHolder{db: q}
	h.db.Find(nil)
	h.db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// aliasParamReuse: the root is an alias-typed parameter.
func aliasParamReuse(db aliasQuery) {
	q := db.Where("x")
	q.Find(nil)
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// =============================================================================
// SHOULD NOT REPORT
// =============================================================================

// aliasPureParam: //gormreuse:pure applies to an alias-typed parameter (a
// signature-invalid directive would be reported as unused instead).
//
//gormreuse:pure
func aliasPureParam(db aliasQuery) {
	_ = db
}

// aliasImmutableReturn: //gormreuse:immutable-return applies to an alias-typed
// receiver-less function returning an alias.
//
//gormreuse:immutable-return
func aliasImmutableReturn(db *aliasDB) aliasQuery {
	return db.Session(&gorm.Session{})
}

func aliasPureCall(db *gorm.DB) {
	q := db.Where("x")
	aliasPureParam(q)
	q.Find(nil) // OK: aliasPureParam is pure
}

func aliasImmutableReturnReuse(db *gorm.DB) {
	q := aliasImmutableReturn(db)
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // OK: q is immutable
}
