package purelib

import (
	"context"

	"gorm.io/gorm"
)

// DB is a global database connection.
var DB *gorm.DB

// Orm is a wrapper type that provides pure DB access.
type Orm struct{}

// DB is a pure method that returns a new *gorm.DB with context.
// This simulates the pattern: new(orm.Orm).DB(ctx)
//
//gormreuse:pure
func (o *Orm) DB(ctx context.Context) *gorm.DB {
	return DB.WithContext(ctx)
}

// GetDB is a pure method that returns a new *gorm.DB.
//
//gormreuse:pure
func (o *Orm) GetDB() *gorm.DB {
	return DB.WithContext(nil)
}

// PureFactory is a pure function that returns a new *gorm.DB.
//
//gormreuse:pure
func PureFactory() *gorm.DB {
	return DB.WithContext(nil)
}
