package before

import (
	"context"
	"maps"

	"gorm.io/gorm"
)

// RangeFuncFilter reassigns q in a range-over-func body and queries it twice
// after the loop, as #341 reports. The value before the loop is immutable, so
// the fix puts a Session after the Where in the body.
func RangeFuncFilter(db *gorm.DB) {
	q := db.Model(&User{}).WithContext(context.Background())
	for k, v := range maps.All(map[string]any{"name": "x"}) {
		q = q.Where(k+" = ?", v)
	}
	q.Where("age > ?", 20).Find(&[]User{})
	q.Find(&[]User{})
}
