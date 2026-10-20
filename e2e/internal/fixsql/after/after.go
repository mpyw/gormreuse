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

// applyTenant changes tx in place, and its callers rely on that: it returns
// nothing through which they could see a change (#243).
func applyTenant(tx *gorm.DB, tenant int) {
	tx.Where("tenant_id = ?", tenant)
	tx.Where("deleted_at IS NULL")
}

// Helper hands q to applyTenant, which adds the tenant filter to q in place
// (#243).
func Helper(db *gorm.DB) {
	q := db.Model(&User{})
	applyTenant(q, 7)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// addTenant discards one statement on tx. It is not a reuse by itself.
func addTenant(tx *gorm.DB) {
	tx.Where("tenant_id = ?", 7)
}

// HelperDiscard hands q to addTenant (#243).
func HelperDiscard(db *gorm.DB) {
	q := db.Model(&User{})
	addTenant(q)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// ClosureArg hands q to a closure that changes it in place (#243).
func ClosureArg(db *gorm.DB) {
	q := db.Model(&User{})
	apply := func(d *gorm.DB) { d.Where("tenant_id = ?", 7) }
	apply(q)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// Slice stores q in a slice and changes it through the slice (#243).
func Slice(db *gorm.DB) {
	q := db.Model(&User{})
	qs := []*gorm.DB{q}
	qs[0].Where("tenant_id = ?", 7)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// Map stores q in a map and changes it through the map (#243).
func Map(db *gorm.DB) {
	q := db.Model(&User{})
	qs := map[string]*gorm.DB{}
	qs["q"] = q
	qs["q"].Where("tenant_id = ?", 7)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// Chan sends q on a channel and changes it through the channel (#243).
func Chan(db *gorm.DB) {
	q := db.Model(&User{})
	ch := make(chan *gorm.DB, 1)
	ch <- q
	(<-ch).Where("tenant_id = ?", 7)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// ErrorRead reads the Error of a chain that changes q in place (#243).
func ErrorRead(db *gorm.DB) {
	q := db.Model(&User{})
	if err := q.Where("tenant_id = ?", 7).Error; err != nil {
		return
	}
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// withTenant adds the tenant filter to tx and returns it.
func withTenant(tx *gorm.DB) *gorm.DB {
	return tx.Where("tenant_id = ?", 7)
}

// ResultDiscarded discards the result of withTenant, which changes q in place
// (#243).
func ResultDiscarded(db *gorm.DB) {
	q := db.Model(&User{})
	_ = withTenant(q)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// EscapeLast hands q to applyTenant after its last query. Nothing sees what
// applyTenant changes, so the root gets a Session (#243).
func EscapeLast(db *gorm.DB) {
	q := db.Model(&User{}).Session(&gorm.Session{})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
	applyTenant(q, 7)
}

// ErrorOfFinisher reads the Error of a query's own result, so the root gets a
// Session (#243).
func ErrorOfFinisher(db *gorm.DB) {
	q := db.Model(&User{}).Session(&gorm.Session{})
	if err := q.Where("name = ?", "x").Find(&[]User{}).Error; err != nil {
		return
	}
	q.Where("name = ?", "y").Find(&[]User{})
}

// tenantAdults changes tx in place and returns a chain on it, which carries
// the change back to the caller. Its discarded statement is reassigned (#243).
func tenantAdults(tx *gorm.DB) *gorm.DB {
	tx = tx.Where("tenant_id = ?", 7).Session(&gorm.Session{})
	tx.Where("name = ?", "x").Find(&[]User{})
	return tx.Where("age > ?", 20)
}

// ParamResult runs a query on the result of tenantAdults.
func ParamResult(db *gorm.DB) {
	tenantAdults(db.Model(&User{})).Find(&[]User{})
}

// ErrorPropagation reads q.Error, which collects the errors of the queries
// that ran on q in place. The second query scans a row of several columns
// into an int, so it fails, and a third query reports the error (#243).
func ErrorPropagation(db *gorm.DB) {
	q := db.Model(&User{})
	q.Find(&[]User{})
	q.Where("age > ?", 20).Table("").Find(new(int))
	if q.Error != nil {
		db.Model(&User{}).Where("failed = ?", true).Find(&[]User{})
	}
}

// LaterLoop discards a statement, then queries in a later loop. The fix
// reassigns the statement and gives it a Session, or each query adds its id
// to the ones before (#311).
func LaterLoop(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q = q.Where("age > ?", 20).Session(&gorm.Session{})
	for _, id := range []int{1, 2, 3} {
		q.Where("id = ?", id).Find(&[]User{})
	}
}

// LaterLoopInIf is LaterLoop with the discarded statement in an if (#311).
func LaterLoopInIf(db *gorm.DB) {
	laterLoopInIf(db, true)
}

func laterLoopInIf(db *gorm.DB, adults bool) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7).Session(&gorm.Session{})
	if adults {
		q = q.Where("age > ?", 20).Session(&gorm.Session{})
	}
	for _, id := range []int{1, 2, 3} {
		q.Where("id = ?", id).Find(&[]User{})
	}
}

// Paginate counts and lists the users that match name, as a list endpoint
// does. The Where in the if merges with db, which is read twice, so it needs
// a Session of its own besides the one at the top of the body, or Count's
// Select and Model leak into Find (#311).
func Paginate(db *gorm.DB) {
	paginate(db.Model(&User{}), "x")
}

func paginate(db *gorm.DB, name string) {
	db = db.Session(&gorm.Session{})
	var total int64
	if name != "" {
		db = db.Where("name = ?", name).Session(&gorm.Session{})
	}
	db.Count(&total)
	db.Offset(10).Limit(10).Find(&[]User{})
}

// PaginateIsolated is Paginate with an isolated caller, so the fix declares
// the parameter immutable-param, and the Where still needs a Session (#311).
func PaginateIsolated(db *gorm.DB) {
	paginateIsolated(db.Model(&User{}).Session(&gorm.Session{}), "x")
}

//gormreuse:immutable-param
func paginateIsolated(db *gorm.DB, name string) {
	var total int64
	if name != "" {
		db = db.Where("name = ?", name).Session(&gorm.Session{})
	}
	db.Count(&total)
	db.Offset(10).Limit(10).Find(&[]User{})
}
