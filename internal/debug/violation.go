package debug

import (
	"go/token"

	"github.com/mpyw/gormreuse/internal/ssa/pollution"
)

var _ Violation = (*violation)(nil)

// Violation represents a violation with debug information.
type Violation interface {
	pollution.Violation
	DebugInfo() *Info
}

// violation is the debug-enabled implementation.
type violation struct {
	pos       token.Pos
	message   string
	debugInfo *Info
}

func (v *violation) Pos() token.Pos   { return v.pos }
func (v *violation) Message() string  { return v.message }
func (v *violation) DebugInfo() *Info { return v.debugInfo }
