// Package fakegorm is a package that a fixture imports under the name gorm,
// so the name gorm in that file does not refer to gorm.io/gorm (#200).
package fakegorm

// Session is not gorm.io/gorm's Session.
type Session struct{}
