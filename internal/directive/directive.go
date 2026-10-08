// Package directive handles gormreuse comment directives.
//
// # Supported Directives
//
// The package supports the following directives:
//
//	//gormreuse:ignore                // Suppress warnings for the next line or same line
//	//gormreuse:pure                  // Mark function/method as not polluting its *gorm.DB argument
//	//gormreuse:immutable-return      // Mark function/method as returning immutable *gorm.DB
//	//gormreuse:immutable-param       // Mark *gorm.DB parameters as immutable (callers pass isolated values)
//	//gormreuse:immutable-input(name) // Mark callback parameter name as receiving an immutable *gorm.DB
//
// Directives can be combined with commas:
//
//	//gormreuse:pure,immutable-return // Both pure and immutable-return
//
// # Syntax
//
// Only //gormreuse:name[,name...] is a directive: a line comment, lowercase
// names, and no space anywhere in it, including after a comma. It is read by
// [ast.ParseDirective]. A reason goes after "//", as in
// "//gormreuse:ignore // reason". Any other comment that starts with
// "gormreuse:" is reported by [FindDirectiveProblems] and has no effect: a malformed
// comment, an unknown name (even one part of a list), a malformed
// immutable-input(name), or text after the name that is not a "//" reason.
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
//declscope:shared // funcset.go keys each DirectiveFuncSet on one
type directiveChecker func(text string) bool

// parseDirective returns the comma-separated parts of a gormreuse directive
// comment, or nil when the comment is not a valid one.
//
//declscope:shared // immutable_input.go splits its immutable-input(name) parts from the same list
func parseDirective(text string) []string {
	parts, _ := readDirective(text)
	return parts
}

// readDirective reads one comment. For a valid gormreuse directive it returns the
// comma-separated parts. For a comment addressed to gormreuse that is not
// valid it returns the problem to report, and the comment has no effect. For
// any other comment both are empty.
//
// Only Go's directive form is read, exactly as [ast.ParseDirective] reads it:
// "//gormreuse:" with no space after "//" or after the colon. The whole list is
// the directive's name, so it holds no space and no empty part. Every part must
// be a name gormreuse reads: one unknown part drops the whole comment. A
// reason goes after "//": the first "//" after the marker ends the directive,
// even with no space before it ("//gormreuse:ignore// reason"). Any other text
// after the name is an error.
func readDirective(text string) ([]string, string) {
	if body, ok := strings.CutPrefix(text, "//"); ok {
		body, _, _ = strings.Cut(body, "//")
		text = "//" + body
	}
	d, ok := ast.ParseDirective(token.NoPos, text)
	if !ok || d.Tool != directiveTool {
		if directiveAddressed(text) {
			return nil, malformedDirectiveMessage
		}
		return nil, ""
	}
	parts := strings.Split(d.Name, ",")
	for _, part := range parts {
		if problem := checkDirectivePart(part); problem != "" {
			return nil, problem
		}
	}
	if d.Args != "" {
		return nil, "gormreuse:" + d.Name + " takes no argument; write a reason after //"
	}
	return parts, ""
}

// directiveNames are the directive names that take no parameter.
var directiveNames = []string{"ignore", "pure", "immutable-return", "immutable-param"}

// checkDirectivePart returns the problem with one part of a directive's list, or "".
func checkDirectivePart(part string) string {
	if part == "" {
		return malformedDirectiveMessage
	}
	if slices.Contains(directiveNames, part) {
		return ""
	}
	if inner, ok := strings.CutPrefix(part, "immutable-input("); ok {
		if name, ok := strings.CutSuffix(inner, ")"); ok && name != "" {
			return ""
		}
		return "malformed gormreuse:" + part + " directive: write it as immutable-input(name)"
	}
	return "unknown directive gormreuse:" + part + " (want ignore, pure, immutable-return, immutable-param or immutable-input(name))"
}

// directiveAddressed reports whether a comment's body, after "//" or "/*" and any
// whitespace, starts with "gormreuse:". Prose that only mentions gormreuse:
// later in a sentence is not addressed to gormreuse.
func directiveAddressed(text string) bool {
	body := text
	if after, ok := strings.CutPrefix(body, "/*"); ok {
		body = strings.TrimSuffix(after, "*/")
	} else {
		body = strings.TrimPrefix(body, "//")
	}
	return strings.HasPrefix(strings.TrimSpace(body), directiveTool+":")
}

// malformedDirectiveMessage is reported on a comment addressed to gormreuse
// that is not in the directive form: a space after "//", after the colon or
// after a comma, a block comment, a name that does not start with [a-z0-9], an
// empty part, or no name at all.
const malformedDirectiveMessage = "malformed gormreuse directive: write it as //gormreuse:name"

// DirectiveProblem is a comment addressed to gormreuse that has no effect, and why.
type DirectiveProblem struct {
	Pos     token.Pos
	Message string
}

// FindDirectiveProblems returns every comment in file that is addressed to gormreuse
// but is not a valid directive: malformed, with an unknown name, or with text
// after the name that is not a "//" reason.
func FindDirectiveProblems(file *ast.File) []DirectiveProblem {
	var out []DirectiveProblem
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if _, problem := readDirective(c.Text); problem != "" {
				out = append(out, DirectiveProblem{Pos: c.Pos(), Message: problem})
			}
		}
	}
	return out
}

// isIgnoreDirective checks if a comment is an ignore directive.
//
//declscope:shared // ignore.go checks comments with it
func isIgnoreDirective(text string) bool { return hasDirective(text, "ignore") }

// isPureDirective checks if a comment contains the pure directive.
// Pure functions don't pollute their *gorm.DB arguments.
//
//declscope:shared // funcset.go checks comments with it
func isPureDirective(text string) bool { return hasDirective(text, "pure") }

// isImmutableReturnDirective checks if a comment contains the immutable-return directive.
// Functions with this directive return immutable *gorm.DB (like Session, WithContext).
//
//declscope:shared // funcset.go checks comments with it
func isImmutableReturnDirective(text string) bool { return hasDirective(text, "immutable-return") }

// isImmutableParamDirective checks if a comment contains the immutable-param directive.
// Functions with this directive assert that their callers guarantee forkable
// (clone>0) *gorm.DB arguments, so the parameter can be reused safely. It is the
// escape hatch for the default-mutable parameter treatment (Phase 1b, #61).
//
//declscope:shared // funcset.go checks comments with it
func isImmutableParamDirective(text string) bool { return hasDirective(text, "immutable-param") }

// hasDirective checks if a comment contains the specified directive.
// Supports comma-separated directives: "//gormreuse:pure,immutable-return".
// Trailing comments use "//": "//gormreuse:ignore // reason here".
func hasDirective(text, name string) bool {
	return slices.Contains(parseDirective(text), name)
}
