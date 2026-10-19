package before

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
	q.Where("deleted_at IS NULL").Limit(10)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// SingleCall discards a statement with one call.
func SingleCall(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q.Where("age > ?", 20)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// SessionOnly discards nothing, so a Session at the root is enough.
func SessionOnly(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// Loop discards a statement after the loop's query. The loop violation is
// reported at the query, before the analyzer has seen the discarded statement.
func Loop(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	for _, x := range []int{1, 2} {
		q.Where("id > ?", x).Find(&[]User{})
		q.Where("age > ?", x)
	}
}

// LoopOnly discards a statement in a loop, and queries after the loop. The
// fix reassigns the statement and gives it a Session.
func LoopOnly(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	for _, x := range []int{1, 2} {
		q.Where("age > ?", x)
	}
	q.Find(&[]User{})
}

// SessionLast discards a chain that ends in a Session. Only q.Where changes q,
// and the fix reassigns the whole statement.
func SessionLast(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	q.Where("age > ?", 20).Session(&gorm.Session{})
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
