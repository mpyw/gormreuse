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
// # Syntax
//
// Only //gormreuse:name[,name...] is a directive: a line comment, lowercase
// names, and no space after "//" or after the colon. It is read by
// [ast.ParseDirective], and a trailing "// reason" is dropped. Any other
// comment that starts with "gormreuse:" is reported with
// [MalformedDirectiveMessage].
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
//	    // Session() first: chaining directly on the parameter (return
//	    // db.Where(...)) consumes the caller's branch and is NOT pure.
//	    return db.Session(&gorm.Session{}).Where("active = ?", true)
//	}
//
// Immutable-return function (returns immutable, like Session/WithContext):
//
//	//gormreuse:pure,immutable-return
//	func GetDB(ctx context.Context) *gorm.DB {
//	    return globalDB.WithContext(ctx)
//	}
package directive

import (
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

// directiveTool is the tool part of every gormreuse directive.
const directiveTool = "gormreuse"

// directiveChecker is a function that checks if a comment is a specific directive.
//
//declscope:package // funcset.go keys each DirectiveFuncSet on one
type directiveChecker func(text string) bool

// parseDirective returns the comma-separated parts of a gormreuse directive
// comment, each trimmed, or nil when the comment is not one.
//
// Only Go's directive form is read, exactly as [ast.ParseDirective] reads it:
// "//gormreuse:" with no space after "//" or after the colon. A trailing
// comment ("//gormreuse:ignore // reason") is dropped.
//
//declscope:package // immutable_input.go splits its immutable-input(name) parts from the same list
func parseDirective(text string) []string {
	d, ok := ast.ParseDirective(token.NoPos, text)
	if !ok || d.Tool != directiveTool {
		return nil
	}
	// ParseDirective splits Name at the first space, so a list written with a
	// space after a comma ("pure, immutable-return") continues into Args.
	list := d.Name
	if d.Args != "" {
		list += " " + d.Args
	}
	// Drop a trailing comment: "pure,immutable-return // note".
	list, _, _ = strings.Cut(list, "//")
	var parts []string
	for part := range strings.SplitSeq(list, ",") {
		parts = append(parts, strings.TrimSpace(part))
	}
	return parts
}

// MalformedDirectiveMessage is reported on every malformed directive.
const MalformedDirectiveMessage = "malformed gormreuse directive: write it as //gormreuse:name"

// IsMalformedDirective reports whether a comment is addressed to gormreuse but
// is not a directive.
//
// A comment is addressed to gormreuse when its body, after "//" or "/*" and
// any whitespace, starts with "gormreuse:". Prose that only mentions
// gormreuse: later in a sentence is not. It is malformed when
// [ast.ParseDirective] does not read it as a gormreuse directive: a space
// after "//" or after the colon, a block comment, a name that does not start
// with [a-z0-9], or no name at all.
func IsMalformedDirective(text string) bool {
	if parseDirective(text) != nil {
		return false
	}
	body := text
	if after, ok := strings.CutPrefix(body, "/*"); ok {
		body = strings.TrimSuffix(after, "*/")
	} else {
		body = strings.TrimPrefix(body, "//")
	}
	return strings.HasPrefix(strings.TrimSpace(body), directiveTool+":")
}

// FindMalformedDirectives returns the position of every comment in file that
// IsMalformedDirective reports.
func FindMalformedDirectives(file *ast.File) []token.Pos {
	var out []token.Pos
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if IsMalformedDirective(c.Text) {
				out = append(out, c.Pos())
			}
		}
	}
	return out
}

// hasDirective checks if a comment contains the specified directive.
// Supports comma-separated directives: "//gormreuse:pure,immutable-return".
// Trailing comments use "//": "//gormreuse:ignore // reason here".
func hasDirective(text, name string) bool {
	return slices.Contains(parseDirective(text), name)
}

// IsIgnoreDirective checks if a comment is an ignore directive.
func IsIgnoreDirective(text string) bool { return hasDirective(text, "ignore") }

// IsPureDirective checks if a comment contains the pure directive.
// Pure functions don't pollute their *gorm.DB arguments.
func IsPureDirective(text string) bool { return hasDirective(text, "pure") }

// IsImmutableReturnDirective checks if a comment contains the immutable-return directive.
// Functions with this directive return immutable *gorm.DB (like Session, WithContext).
func IsImmutableReturnDirective(text string) bool { return hasDirective(text, "immutable-return") }

// IsImmutableParamDirective checks if a comment contains the immutable-param directive.
// Functions with this directive assert that their callers guarantee forkable
// (clone>0) *gorm.DB arguments, so the parameter can be reused safely. It is the
// escape hatch for the default-mutable parameter treatment (Phase 1b, #61).
func IsImmutableParamDirective(text string) bool { return hasDirective(text, "immutable-param") }
