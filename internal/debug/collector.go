package debug

import (
	"go/token"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/typeutil"
)

// Collector encapsulates debug information collection.
// This keeps debug logic isolated from the main analysis code.
type Collector struct {
	rootDebugInfo map[ssa.Value]RootInfo
	usagesByPos   map[token.Pos][]usageEntry
}

// usageEntry holds debug info for a single usage.
type usageEntry struct {
	root       ssa.Value
	methodName string
	callType   string
}

// NewCollector creates a new Collector.
func NewCollector() *Collector {
	return &Collector{
		rootDebugInfo: make(map[ssa.Value]RootInfo),
		usagesByPos:   make(map[token.Pos][]usageEntry),
	}
}

// RecordUsage stores debug info for a usage site.
// This is called from handlers to record method names and call types.
func (c *Collector) RecordUsage(root ssa.Value, pos token.Pos, methodName, callType string) {
	// Store root info if not already stored
	if _, exists := c.rootDebugInfo[root]; !exists {
		c.rootDebugInfo[root] = NewRootInfo(root)
	}

	// Store usage debug info indexed by position
	c.usagesByPos[pos] = append(c.usagesByPos[pos], usageEntry{
		root:       root,
		methodName: methodName,
		callType:   callType,
	})
}

// BuildViolationInfoByPos builds debug info by looking up all usages at the given position.
// This is used when we only have the violation position and need to find the associated debug info.
func (c *Collector) BuildViolationInfoByPos(pos token.Pos) *Info {
	// Direct O(1) lookup by position
	entries := c.usagesByPos[pos]
	if len(entries) == 0 {
		return nil
	}

	// Use the first entry's root for the root info
	root := entries[0].root
	rootInfo, ok := c.rootDebugInfo[root]
	if !ok {
		rootInfo = NewRootInfo(root)
	}

	// Build branches from all entries at this position
	branches := make([]BranchInfo, 0, len(entries))
	for _, entry := range entries {
		branches = append(branches, BranchInfo{
			Pos:        pos,
			MethodName: entry.methodName,
			IsFinisher: typeutil.IsFinisherBuiltin(entry.methodName),
			CallType:   entry.callType,
			Context:    []string{},
		})
	}

	return &Info{
		Root:     rootInfo,
		Branches: branches,
	}
}
