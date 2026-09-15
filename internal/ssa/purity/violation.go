package purity

import "go/token"

// =============================================================================
// Violation
// =============================================================================

// Violation represents a contract violation found by this package's
// validators (Validator.Validate, ValidateImmutableReturn, ValidateImmutableInputs).
type Violation struct {
	Pos     token.Pos
	Message string

	// Leak is true when the violation is a definitive escape of the argument
	// (channel send, slice/array store, map store) rather than a conservative
	// guess (passing to a not-yet-proven-pure function). Only definitive escapes
	// revoke the function's pure-trust at its call sites — a conservative
	// func-arg violation might still be pure in practice, and cascading it would
	// produce false positives (see nestedClosureOuterPureViolation).
	Leak bool
}
