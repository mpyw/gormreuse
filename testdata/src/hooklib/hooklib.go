// Package hooklib declares a struct with a func field in another package
// than the fixtures that build it (#397).
package hooklib

import "gorm.io/gorm"

// Config holds a builder.
type Config struct {
	Hook func() *gorm.DB
}

// Run calls the builder and chains on the result.
func Run(c Config) {
	c.Hook().Find(nil)
}
