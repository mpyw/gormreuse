package directive

import "go/types"

// =============================================================================
// Signature Helpers
// =============================================================================

// signatureValidator checks if a function signature is valid for the directive.
// For pure: returns true if any parameter contains *gorm.DB
// For immutable-return: returns true if any return value contains *gorm.DB
//
//declscope:package // funcset.go validates directive targets with it
type signatureValidator func(*types.Signature) bool

// asFunctionSignature returns the *types.Signature for a parameter that is itself
// a function, or nil if it isn't (or if type info is unavailable — an honest U2
// in that degraded case rather than a misleading U3).
//
//declscope:package // immutable_input.go resolves callback parameter types with it
func asFunctionSignature(t types.Type) *types.Signature {
	if t == nil {
		return nil
	}
	sig, _ := t.Underlying().(*types.Signature)
	return sig
}
