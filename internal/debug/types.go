package debug

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// Info contains collected debug information for a violation.
type Info struct {
	Root     RootInfo
	Branches []BranchInfo
}

// BranchInfo contains information about a single branch usage.
type BranchInfo struct {
	Pos        token.Pos
	MethodName string
	IsFinisher bool
	CallType   string
	Context    []string // Control flow context: ["for-range", "if"], etc.
}

// RootInfo contains information about a mutable root.
type RootInfo struct {
	Pos       token.Pos
	VarName   string
	IsMutable bool
	SSAValue  string
}

// NewRootInfo creates RootInfo from an SSA value.
func NewRootInfo(root ssa.Value) RootInfo {
	return RootInfo{
		Pos:       root.Pos(),
		VarName:   root.Name(),
		IsMutable: true, // Assume mutable if we're tracking it
		SSAValue:  root.String(),
	}
}
