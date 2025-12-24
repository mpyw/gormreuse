// Package directive handles gormreuse comment directives.
//
// # Supported Directives
//
// The package supports the following directives:
//
//	//gormreuse:ignore           - Suppress warnings for the next line or same line
//	//gormreuse:pure             - Mark function/method as not polluting its *gorm.DB argument
//	//gormreuse:immutable-return - Mark function/method as returning immutable *gorm.DB
//
// Directives can be combined with commas:
//
//	//gormreuse:pure,immutable-return - Both pure and immutable-return
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
// Pure function (doesn't pollute arguments):
//
//	//gormreuse:pure
//	func applyFilters(db *gorm.DB) *gorm.DB {
//	    return db.Where("active = ?", true)
//	}
//
// Immutable-return function (returns immutable, like Session/WithContext):
//
//	//gormreuse:pure,immutable-return
//	func GetDB(ctx context.Context) *gorm.DB {
//	    return globalDB.WithContext(ctx)
//	}
package directive

import "strings"

const directivePrefix = "gormreuse:"

// hasDirective checks if a comment contains the specified directive.
// Supports comma-separated directives: "//gormreuse:pure,immutable-return".
// Trailing comments use "//": "//gormreuse:ignore // reason here".
func hasDirective(text, name string) bool {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, directivePrefix) {
		return false
	}
	// Extract directive part after prefix
	text = strings.TrimPrefix(text, directivePrefix)

	// Split off trailing comment (// ...)
	// e.g., "ignore // reason" -> "ignore"
	// e.g., "pure,immutable-return // note" -> "pure,immutable-return"
	if idx := strings.Index(text, "//"); idx != -1 {
		text = text[:idx]
	}
	text = strings.TrimSpace(text)

	// Split by comma and check each
	for _, part := range strings.Split(text, ",") {
		if strings.TrimSpace(part) == name {
			return true
		}
	}
	return false
}

// IsIgnoreDirective checks if a comment is an ignore directive.
func IsIgnoreDirective(text string) bool { return hasDirective(text, "ignore") }

// IsPureDirective checks if a comment contains the pure directive.
// Pure functions don't pollute their *gorm.DB arguments.
func IsPureDirective(text string) bool { return hasDirective(text, "pure") }

// IsImmutableReturnDirective checks if a comment contains the immutable-return directive.
// Functions with this directive return immutable *gorm.DB (like Session, WithContext).
func IsImmutableReturnDirective(text string) bool { return hasDirective(text, "immutable-return") }
