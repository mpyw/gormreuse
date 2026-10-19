package gorm

// This file is separate from gorm.go so that the tests that copy gorm.go
// alone into a module need no clause package.

import "gorm.io/gorm/clause"

// Expr returns a raw SQL expression with its variables.
func Expr(expr string, args ...interface{}) clause.Expr {
	return clause.Expr{SQL: expr, Vars: args}
}
