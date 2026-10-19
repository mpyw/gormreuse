// Code generated from ../before/before.go by TestGoldenFilesE2E in the main module. DO NOT EDIT.

package after

import "gorm.io/gorm"

// User is the model the fixtures query.
type User struct {
	ID   uint
	Name string
	Age  int
}

// MultiCall discards a statement with several calls. The fix reassigns the
// whole statement and puts the Session after its last call.
func MultiCall(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q = q.Where("deleted_at IS NULL").Limit(10).Session(&gorm.Session{})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// SingleCall discards a statement with one call.
func SingleCall(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q = q.Where("age > ?", 20).Session(&gorm.Session{})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// SessionOnly discards nothing, so a Session at the root is enough.
func SessionOnly(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// Loop discards a statement after the loop's query. The loop violation is
// reported at the query, before the analyzer has seen the discarded statement.
func Loop(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
	for _, x := range []int{1, 2} {
		q.Where("id > ?", x).Find(&[]User{})
		q = q.Where("age > ?", x).Session(&gorm.Session{})
	}
}

// LoopOnly discards a statement in a loop, and queries after the loop. The
// fix reassigns the statement and gives it a Session.
func LoopOnly(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	for _, x := range []int{1, 2} {
		q = q.Where("age > ?", x).Session(&gorm.Session{})
	}
	q.Find(&[]User{})
}

// SessionLast discards a chain that ends in a Session. Only q.Where changes q,
// and the fix reassigns the whole statement.
func SessionLast(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q = q.Where("age > ?", 20).Session(&gorm.Session{})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// Closure discards a statement in a closure called at one later site. The
// closure captures q, so q is not reassigned, and no fix is offered.
func Closure(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	f := func() {
		q.Where("age > ?", 20)
	}
	q.Where("name = ?", "x").Find(&[]User{})
	f()
	q.Where("name = ?", "y").Find(&[]User{})
}

// MethodValue queries through a method value of q and discards nothing, so a
// Session at the root is enough (#178).
func MethodValue(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
	where := q.Where
	where("name = ?", "x").Find(&[]User{})
	where("name = ?", "y").Find(&[]User{})
}

// MethodValueDiscarded discards a call through a method value of q. It changes
// q in place, a Session at the root would drop it, and the call names no
// variable to reassign, so no fix is offered (#178, #199).
func MethodValueDiscarded(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	where := q.Where
	where("age > ?", 20)
	where("name = ?", "x").Find(&[]User{})
	where("name = ?", "y").Find(&[]User{})
}

// Reassign is the README's reassignment example: the reassigned value is
// used once, so it needs no Session (#202).
func Reassign(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q = q.Where("age > ?", 20)
	q.Where("name = ?", "x").Find(&[]User{})
}

// Combined is the README's combined example: only the first reassigned value
// is used twice (#202).
func Combined(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q = q.Where("age > ?", 20).Session(&gorm.Session{})
	q.Where("name = ?", "x").Find(&[]User{})
	q = q.Where("age < ?", 60)
	q.Where("name = ?", "y").Find(&[]User{})
}

// ifWithoutElse discards a statement in an if without an else. On the path
// that skips it, the original root is used twice, so it needs a Session as
// well as the reassignment (#202).
func ifWithoutElse(db *gorm.DB, c bool) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
	if c {
		q = q.Where("age > ?", 20).Session(&gorm.Session{})
	}
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// IfWithoutElseTaken runs ifWithoutElse through its if.
func IfWithoutElseTaken(db *gorm.DB) { ifWithoutElse(db, true) }

// IfWithoutElseSkipped runs ifWithoutElse past its if.
func IfWithoutElseSkipped(db *gorm.DB) { ifWithoutElse(db, false) }

// phiAfterUses uses the root twice, then merges it with another value that
// is used once. The root needs a Session although the merge does not (#202).
func phiAfterUses(db *gorm.DB, c bool) {
	db = db.Session(&gorm.Session{})
	q := db.Model(&User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
	if c {
		q = db.Model(&User{}).Where("tenant_id = ?", 8)
	}
	q.Where("name = ?", "z").Find(&[]User{})
}

// PhiAfterUsesTaken runs phiAfterUses through its if.
func PhiAfterUsesTaken(db *gorm.DB) { phiAfterUses(db, true) }

// PhiAfterUsesSkipped runs phiAfterUses past its if.
func PhiAfterUsesSkipped(db *gorm.DB) { phiAfterUses(db, false) }
