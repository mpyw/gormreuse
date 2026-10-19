package purity

import "go/token"

// =============================================================================
// Violation
// =============================================================================

// Violation represents a contract violation found by this package's
// validators (Validator.Validate, ImmutableReturnValidator.Validate, ValidateImmutableInputs).
type Violation struct {
	Pos     token.Pos
	Message string

	// Definitive is true when the violation certainly breaks the contract: the
	// argument is polluted by a gorm chain method, escapes (channel send,
	// slice/array/map store, store outside the function's locals), or is passed
	// to a pure function that itself fails validation. It is false for a
	// conservative guess (passing to a function not marked pure). Only definitive
	// violations revoke the function's pure-trust at its call sites — a
	// conservative func-arg violation might still be pure in practice, and
	// cascading it would produce false positives (see
	// nestedClosureOuterPureViolation).
	Definitive bool
}
