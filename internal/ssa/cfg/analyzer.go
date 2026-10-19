// Package cfg provides control flow graph analysis for gormreuse.
//
// # Overview
//
// This package analyzes the Control Flow Graph (CFG) of SSA functions to:
//   - Detect loops (for special handling of loop-external roots)
//   - Check block reachability (for violation detection)
//
// # CFG Concepts
//
// An SSA function has a CFG where:
//
//   - Nodes are BasicBlocks (sequences of instructions)
//
//   - Edges are control flow transitions (jumps, branches, returns)
//
//     ┌─────────────────────────────────────────────────────────────────────────┐
//     │                    Example: if-else CFG                                 │
//     │                                                                         │
//     │                    ┌─────────────┐                                      │
//     │                    │  Entry (0)  │                                      │
//     │                    │  if cond    │                                      │
//     │                    └──────┬──────┘                                      │
//     │                    true /   \ false                                     │
//     │                        /     \                                          │
//     │              ┌────────▼───┐ ┌─▼────────┐                                │
//     │              │  Then (1)  │ │ Else (2) │                                │
//     │              └────────┬───┘ └─┬────────┘                                │
//     │                       \     /                                           │
//     │                        \   /                                            │
//     │                    ┌────▼─▼────┐                                        │
//     │                    │  Exit (3) │                                        │
//     │                    │ (Phi)     │                                        │
//     │                    └───────────┘                                        │
//     └─────────────────────────────────────────────────────────────────────────┘
//
// # Loop Detection
//
// A loop exists when there's a back-edge in the CFG (edge to a block with
// lower index that can reach the source):
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│                    Example: for loop CFG                                │
//	│                                                                         │
//	│                    ┌─────────────┐                                      │
//	│                    │  Entry (0)  │                                      │
//	│                    └──────┬──────┘                                      │
//	│                           │                                             │
//	│                    ┌──────▼──────┐◀───────┐                             │
//	│                    │  Header (1) │        │  back-edge                  │
//	│                    │  for cond   │        │  (loop)                     │
//	│                    └──────┬──────┘        │                             │
//	│                    true /   \ false       │                             │
//	│                        /     \            │                             │
//	│              ┌────────▼───┐ ┌─▼────────┐  │                             │
//	│              │  Body (2)  │ │ Exit (3) │  │                             │
//	│              └────────┬───┘ └──────────┘  │                             │
//	│                       │                   │                             │
//	│                       └───────────────────┘                             │
//	└─────────────────────────────────────────────────────────────────────────┘
package cfg

import (
	"golang.org/x/tools/go/ssa"
)

// Analyzer provides control flow graph analysis for SSA functions.
//
// It is stateless and can be reused across multiple analyses.
// All analysis results are returned as new objects, not stored internally.
type Analyzer struct{}

// NewAnalyzer creates a new Analyzer.
func NewAnalyzer() *Analyzer {
	return &Analyzer{}
}

// CanReach checks if srcBlock can reach dstBlock in the CFG using BFS.
//
// This is used for violation detection: if an earlier use of a mutable root
// can reach a later use, the later use is a violation.
//
// Algorithm: Breadth-first search through successor edges.
//
// Time complexity: O(V + E) where V = blocks, E = edges
func (a *Analyzer) CanReach(src, dst *ssa.BasicBlock) bool {
	if src == nil || dst == nil {
		return false
	}
	if src == dst {
		return true // Same block is trivially reachable
	}

	// BFS traversal
	visited := make(map[*ssa.BasicBlock]bool)
	queue := []*ssa.BasicBlock{src}
	visited[src] = true

	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]

		for _, succ := range block.Succs {
			if succ == dst {
				return true
			}
			if !visited[succ] {
				visited[succ] = true
				queue = append(queue, succ)
			}
		}
	}
	return false
}
