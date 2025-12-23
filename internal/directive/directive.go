// Package directive handles gormreuse comment directives.
//
// Supported directives:
//   - //gormreuse:ignore - Suppress warnings for the next line or same line
//   - //gormreuse:pure - Mark function/method as not polluting its *gorm.DB argument
package directive

import "strings"

const directivePrefix = "gormreuse:"

// hasDirective checks if a comment contains the specified directive.
// Supports both "//gormreuse:name" and "// gormreuse:name".
func hasDirective(text, name string) bool {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, directivePrefix+name)
}

// IsIgnoreDirective checks if a comment is an ignore directive.
func IsIgnoreDirective(text string) bool { return hasDirective(text, "ignore") }

// IsPureDirective checks if a comment is a pure directive.
func IsPureDirective(text string) bool { return hasDirective(text, "pure") }
