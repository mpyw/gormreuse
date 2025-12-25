package pollution

import "go/token"

// Violation represents a detected reuse violation.
type Violation interface {
	Pos() token.Pos
	Message() string
}

// violation is the basic implementation without debug info.
type violation struct {
	pos     token.Pos
	message string
}

func (v *violation) Pos() token.Pos  { return v.pos }
func (v *violation) Message() string { return v.message }
