package internal

import "gorm.io/gorm"

// GetDB returns the global DB instance.
// This is used by noimport package to test fix generation without gorm import.
func GetDB() *gorm.DB {
	return DB
}
