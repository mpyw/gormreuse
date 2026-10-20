package before

import (
	"fmt"
	"sort"

	"gorm.io/gorm"
)

// pointerApply adds the tenant filter to *p in place (#345).
func pointerApply(p **gorm.DB) { (*p).Where("tenant_id = ?", 7) }

// pointerApplyAll adds the tenant filter to each *p in place (#345).
func pointerApplyAll(ps []**gorm.DB) {
	for _, p := range ps {
		(*p).Where("tenant_id = ?", 7)
	}
}

// pointerSvc holds a *gorm.DB that apply changes in place.
type pointerSvc struct{ db *gorm.DB }

func (s *pointerSvc) apply() { s.db.Where("tenant_id = ?", 7) }

// pointerShared is reached by code that runs inside a call (#345).
var pointerShared pointerSvc

// pointerSorter changes pointerShared.db in place from Len.
type pointerSorter []int

func (s pointerSorter) Len() int           { pointerShared.apply(); return len(s) }
func (s pointerSorter) Less(i, j int) bool { return s[i] < s[j] }
func (s pointerSorter) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// PointerAddr hands the address of q to a helper that changes q in place
// (#345).
func PointerAddr(db *gorm.DB) {
	q := db.Model(&User{})
	pointerApply(&q)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// PointerSlice hands the address of q on in a slice (#345).
func PointerSlice(db *gorm.DB) {
	q := db.Model(&User{})
	pointerApplyAll([]**gorm.DB{&q})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// PointerConnection stores q where the callback of Connection reaches it
// (#345).
func PointerConnection(db *gorm.DB) {
	q := db.Model(&User{})
	pointerShared.db = q
	_ = db.Session(&gorm.Session{}).Connection(func(*gorm.DB) error { pointerShared.apply(); return nil })
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// PointerScopes stores q where a scope of Scopes reaches it (#345).
func PointerScopes(db *gorm.DB) {
	q := db.Model(&User{})
	pointerShared.db = q
	db.Session(&gorm.Session{}).Scopes(func(tx *gorm.DB) *gorm.DB { pointerShared.apply(); return tx }).Find(&[]User{})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// PointerSort stores q where a method that sort.Sort calls reaches it
// (#345).
func PointerSort(db *gorm.DB) {
	q := db.Model(&User{})
	pointerShared.db = q
	sort.Sort(pointerSorter{2, 1})
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}

// PointerPrint stores q where nothing reaches it before the queries: fmt.Sprint
// of an int runs no code of this package. It still gets a fix.
func PointerPrint(db *gorm.DB) {
	q := db.Model(&User{}).Where("tenant_id = ?", 7)
	pointerShared.db = q
	_ = fmt.Sprint(1)
	q.Where("name = ?", "x").Find(&[]User{})
	q.Where("name = ?", "y").Find(&[]User{})
}
