// Package directive handles gormreuse comment directives.
//
// # Supported Directives
//
// The package supports two directive types:
//
//	//gormreuse:ignore - Suppress warnings for the next line or same line
//	//gormreuse:pure   - Mark function/method as not polluting its *gorm.DB argument
//
// # Directive Placement
//
// Directives can be placed:
//   - On the line before the affected code (most common)
//   - On the same line as the affected code
//   - On a function declaration (function-level ignore/pure)
//   - Before the package declaration (file-level ignore)
//
// # Examples
//
// Line-level ignore:
//
//	//gormreuse:ignore
//	q.Find(nil)  // This violation is suppressed
//
// Same-line ignore:
//
//	q.Find(nil)  //gormreuse:ignore
//
// Function-level ignore:
//
//	//gormreuse:ignore
//	func legacy() {
//	    // All violations in this function are suppressed
//	}
//
// Pure function marking:
//
//	//gormreuse:pure
//	func safeQuery(db *gorm.DB) *gorm.DB {
//	    return db.Session(&gorm.Session{})
//	}
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
