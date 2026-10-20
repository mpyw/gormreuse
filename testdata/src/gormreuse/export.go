package internal

import "gorm.io/gorm"

// GetDB returns the global DB instance. It only reads the global, so it is
// inferred immutable-return, like a read of DB itself (#308).
func GetDB() *gorm.DB { // want GetDB:"gormreuse immutable-return"
	return DB
}

// GetQuery returns a chain on the global DB instance, a mutable root.
// This is used by noimport package to test fix generation without gorm import.
func GetQuery() *gorm.DB {
	return DB.Where("deleted_at IS NULL")
}
