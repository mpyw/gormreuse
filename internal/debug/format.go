package debug

import (
	"fmt"
	"go/token"
	"strings"
)

// FormatViolation returns a formatted debug string for a violation.
func FormatViolation(funcName string, info *Info, fset *token.FileSet) string {
	if info == nil {
		return ""
	}

	var buf strings.Builder

	// Function header
	fmt.Fprintf(&buf, "Function: %s\n", funcName)

	// Root information
	rootPos := fset.Position(info.Root.Pos)
	fmt.Fprintf(&buf, "  Root: line %d\n", rootPos.Line)
	if info.Root.VarName != "" {
		fmt.Fprintf(&buf, "    %s := ...\n", info.Root.VarName)
	}
	if info.Root.IsMutable {
		fmt.Fprintf(&buf, "    └─ mutable\n")
	} else {
		fmt.Fprintf(&buf, "    └─ immutable\n")
	}

	// Branches
	if len(info.Branches) > 0 {
		fmt.Fprintf(&buf, "\n  Branches:\n")
		for i, branch := range info.Branches {
			pos := fset.Position(branch.Pos)
			fmt.Fprintf(&buf, "    %d. line %d", i+1, pos.Line)
			if branch.MethodName != "" {
				fmt.Fprintf(&buf, ": %s", branch.MethodName)
			}
			fmt.Fprintf(&buf, "\n")

			if branch.MethodName != "" {
				fmt.Fprintf(&buf, "       ├─ method: %s\n", branch.MethodName)
			}

			finisherStr := "no"
			if branch.IsFinisher {
				finisherStr = "yes"
			}
			fmt.Fprintf(&buf, "       ├─ finisher: %s\n", finisherStr)

			if branch.CallType != "" {
				fmt.Fprintf(&buf, "       ├─ call type: %s\n", branch.CallType)
			}

			if len(branch.Context) > 0 {
				fmt.Fprintf(&buf, "       └─ context: %s\n", strings.Join(branch.Context, " → "))
			} else {
				fmt.Fprintf(&buf, "       └─ context: (none)\n")
			}
		}
	}

	// TODO: Add fix strategy
	fmt.Fprintf(&buf, "\n  Fix: (strategy TBD)\n")

	return buf.String()
}
