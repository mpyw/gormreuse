package internal

import "gorm.io/gorm"

// =============================================================================
// SHOULD REPORT - Comments addressed to gormreuse that are not directives
// =============================================================================

// gormreuse:pure // want `malformed gormreuse directive: write it as //gormreuse:name`
func malformedSpaceAfterMarker(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

//gormreuse: pure // want `malformed gormreuse directive: write it as //gormreuse:name`
func malformedSpaceAfterColon(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

/*gormreuse:pure*/ // want `malformed gormreuse directive: write it as //gormreuse:name`
func malformedBlockForm(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

//gormreuse:Pure // want `malformed gormreuse directive: write it as //gormreuse:name`
func malformedUppercaseName(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

//gormreuse: // want `malformed gormreuse directive: write it as //gormreuse:name`
func malformedNoName() {}

//gormreuse:pure, immutable-return // want `malformed gormreuse directive: write it as //gormreuse:name`
func malformedSpaceAfterComma(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

//gormreuse:pure, // want `malformed gormreuse directive: write it as //gormreuse:name`
func malformedTrailingComma(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

// A malformed ignore suppresses nothing.
func malformedIgnoreSuppressesNothing(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	// gormreuse:ignore // want `malformed gormreuse directive: write it as //gormreuse:name`
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// =============================================================================
// SHOULD REPORT - Directives with a name gormreuse does not read (#156)
// =============================================================================
//
// The whole comment has no effect, even when only one part of a list is
// unknown, so the reuse below each one is still reported.

func unknownIgnore(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	//gormreuse:ignor // want `^unknown directive gormreuse:ignor \(want ignore, pure, immutable-return, immutable-param or immutable-input\(name\)\)$`
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func unknownPartOfIgnoreList(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	//gormreuse:ignore,ignor // want `^unknown directive gormreuse:ignor `
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

//gormreuse:pur // want `^unknown directive gormreuse:pur `
func unknownPure(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", 1)
}

func callsUnknownPure(db *gorm.DB) {
	unknownPure(db).Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

//gormreuse:pure,imutable-return // want `^unknown directive gormreuse:imutable-return `
func unknownPartOfPureList(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{}).Where("tenant_id = ?", 1)
}

func callsUnknownPartOfPureList(db *gorm.DB) {
	unknownPartOfPureList(db).Find(nil)
	db.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

//gormreuse:imutable-input(cb) // want `^unknown directive gormreuse:imutable-input\(cb\) `
func unknownImmutableInput(cb func(*gorm.DB), db *gorm.DB) {
	cb(db.Session(&gorm.Session{}))
}

func callsUnknownImmutableInput(db *gorm.DB) {
	unknownImmutableInput(func(q *gorm.DB) {
		q.Find(nil)
		q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}, db)
}

// =============================================================================
// SHOULD REPORT - Malformed immutable-input(name) (#156)
// =============================================================================

//gormreuse:immutable-input(cb // want `^malformed gormreuse:immutable-input\(cb directive: write it as immutable-input\(name\)$`
func unclosedImmutableInput(cb func(*gorm.DB), db *gorm.DB) {
	cb(db.Session(&gorm.Session{}))
}

func callsUnclosedImmutableInput(db *gorm.DB) {
	unclosedImmutableInput(func(q *gorm.DB) {
		q.Find(nil)
		q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}, db)
}

//gormreuse:immutable-input() // want `^malformed gormreuse:immutable-input\(\) directive: write it as immutable-input\(name\)$`
func emptyImmutableInput(cb func(*gorm.DB), db *gorm.DB) {
	cb(db.Session(&gorm.Session{}))
}

// =============================================================================
// SHOULD REPORT - Text after the name that is not a "//" reason (#156)
// =============================================================================

func dashReason(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	//gormreuse:ignore - reason // want `^gormreuse:ignore takes no argument; write a reason after //$`
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func wordsReason(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	//gormreuse:ignore intentional reuse // want `^gormreuse:ignore takes no argument; write a reason after //$`
	q.Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

//gormreuse:pure,immutable-return - fresh session // want `^gormreuse:pure,immutable-return takes no argument; write a reason after //$`
func dashReasonOnList(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

// =============================================================================
// SHOULD NOT REPORT - Canonical directives, and comments not addressed to gormreuse
// =============================================================================

//gormreuse:pure,immutable-return
func canonicalCommaList(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

//gormreuse:pure // returns a fresh session
func canonicalTrailingReason(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{})
}

//gormreuse:immutable-input(cb)
func canonicalImmutableInput(db *gorm.DB, cb func(tx *gorm.DB)) {
	cb(db.Session(&gorm.Session{}))
}

func canonicalIgnore(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	q.Count(nil) //gormreuse:ignore // intentional
}

func canonicalReasonWithoutSpaceAfterSlashes(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	//gormreuse:ignore //intentional
	q.Count(nil)
}

func canonicalReasonWithoutSpaceBeforeSlashes(db *gorm.DB) {
	q := db.Where("active = ?", true)
	q.Find(nil)
	//gormreuse:ignore// intentional
	q.Count(nil)
}

//gormreuse:immutable-input(cb) // passes a fresh session
func canonicalImmutableInputWithReason(cb func(*gorm.DB), db *gorm.DB) {
	cb(db.Session(&gorm.Session{}))
}

func callsCanonicalImmutableInput(db *gorm.DB) {
	canonicalImmutableInputWithReason(func(q *gorm.DB) {
		q.Find(nil)
		q.Count(nil)
	}, db)
}

// gormreusex:pure
func lookalikeTool() {}

// The linter reads gormreuse:pure only as a directive, so this prose is fine.
func proseMentioningTheTool() {}
