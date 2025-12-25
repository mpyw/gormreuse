package pollution

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// DebugCollector interface for optional debug information collection.
// Only debug.Tracker implements this interface.
type DebugCollector interface {
	CollectDebugInfo(root ssa.Value, pos token.Pos, methodName, callType string)
}
