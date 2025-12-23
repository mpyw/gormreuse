// Package directive handles gormreuse comment directives.
//
// Supported directives:
//   - //gormreuse:ignore - Suppress warnings for the next line or same line
//   - //gormreuse:pure - Mark function/method as not polluting its *gorm.DB argument
package directive

import (
	"strings"
)

// IsIgnoreDirective checks if a comment is an ignore directive.
// Supports both "//gormreuse:ignore" and "// gormreuse:ignore".
func IsIgnoreDirective(text string) bool {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "gormreuse:ignore")
}

// IsPureDirective checks if a comment is a pure directive.
// Supports both "//gormreuse:pure" and "// gormreuse:pure".
// Functions marked pure are assumed NOT to pollute *gorm.DB arguments.
func IsPureDirective(text string) bool {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "gormreuse:pure")
}
