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

	"github.com/mpyw/gormreuse/internal/effort"
)

// Analyzer provides control flow graph analysis for SSA functions.
//
// It caches what it finds for each function: the blocks each block reaches,
// the loops and the Repeats. One Analyzer is shared by everything that asks
// about the functions of a pass, the checker and the suggested fixes, so each
// question is answered once (#299). The functions must not change while it
// is in use, and it is not safe for concurrent use.
type Analyzer struct {
	graphs map[*ssa.Function]*analyzerGraph
}

// analyzerGraph is what an Analyzer has found for one function.
type analyzerGraph struct {
	// reach holds, for the block of each index, the set of blocks it
	// reaches, itself included, as a bitset indexed by block index. A set is
	// nil until a block is asked about.
	reach [][]uint64

	// avoiding holds, for a source block and a block to avoid, the blocks
	// that the source reaches without passing through that block
	// (CanReachAvoiding), as a bitset like reach.
	avoiding map[[2]int][]uint64

	//declscope:shared // loop.go's DetectLoops fills it
	loops *LoopInfo

	// The Repeats of the function analysed on its own, and of the function
	// as a closure of each parent Repeats, and the blocks where it is
	// declared and runs in the function that declares it.
	//declscope:shared // repeat.go fills it
	repeats *Repeats
	//declscope:shared // repeat.go fills it
	closures map[*Repeats]*Repeats
	//declscope:shared // repeat.go fills it
	sites *repeatSites
}

// NewAnalyzer creates a new Analyzer with nothing cached.
func NewAnalyzer() *Analyzer {
	return &Analyzer{graphs: make(map[*ssa.Function]*analyzerGraph)}
}

// graphOf returns the cache of fn, creating it.
//
//declscope:shared // loop.go and repeat.go cache their results in it
func (a *Analyzer) graphOf(fn *ssa.Function) *analyzerGraph {
	g, ok := a.graphs[fn]
	if !ok {
		g = &analyzerGraph{}
		a.graphs[fn] = g
	}
	return g
}

// CanReach checks if srcBlock can reach dstBlock in the CFG.
//
// This is used for violation detection: if an earlier use of a mutable root
// can reach a later use, the later use is a violation.
//
// The blocks that src reaches are found once, by a breadth-first search
// through successor edges, and kept, so later questions about src take
// constant time. Blocks that are not those of fn.Blocks at their index, such
// as blocks built by hand, are searched each time.
func (a *Analyzer) CanReach(src, dst *ssa.BasicBlock) bool {
	if src == nil || dst == nil {
		return false
	}
	if src == dst {
		return true // Same block is trivially reachable
	}
	fn := src.Parent()
	if !indexedByAnalyzer(fn, src) || dst.Parent() != fn || !indexedByAnalyzer(fn, dst) {
		return reachOutsideAnalyzer(src, dst)
	}
	return a.reachSet(fn, src)[dst.Index/64]&(1<<(dst.Index%64)) != 0
}

// indexedByAnalyzer reports whether b is the block of fn.Blocks at b.Index.
func indexedByAnalyzer(fn *ssa.Function, b *ssa.BasicBlock) bool {
	return fn != nil && b.Index >= 0 && b.Index < len(fn.Blocks) && fn.Blocks[b.Index] == b
}

// reachSet returns the blocks that src, a block of fn at its index, reaches,
// itself included.
func (a *Analyzer) reachSet(fn *ssa.Function, src *ssa.BasicBlock) []uint64 {
	g := a.graphOf(fn)
	if g.reach == nil {
		g.reach = make([][]uint64, len(fn.Blocks))
	}
	if set := g.reach[src.Index]; set != nil {
		return set
	}
	set := make([]uint64, (len(fn.Blocks)+63)/64)
	set[src.Index/64] |= 1 << (src.Index % 64)
	// Each block enters the queue once, so it is made at its full size and
	// read by position. Reslicing its head off made the appends reallocate
	// it again and again, for every src of fn (#300).
	queue := make([]*ssa.BasicBlock, 1, len(fn.Blocks))
	queue[0] = src
	for i := 0; i < len(queue); i++ {
		for _, succ := range queue[i].Succs {
			effort.Step()
			if set[succ.Index/64]&(1<<(succ.Index%64)) == 0 {
				set[succ.Index/64] |= 1 << (succ.Index % 64)
				queue = append(queue, succ)
			}
		}
	}
	g.reach[src.Index] = set
	return set
}

// CanReachAvoiding reports whether src can reach dst on a path that does not
// enter avoid. The path starts at the end of src and ends at the start of
// dst: src and dst may be avoid itself, and a block reaches itself. It is how
// the tracker asks whether two uses of a root that a call in avoid defines
// may see the same run of the call (#375).
func (a *Analyzer) CanReachAvoiding(src, dst, avoid *ssa.BasicBlock) bool {
	if src == nil || dst == nil {
		return false
	}
	if src == dst {
		return true
	}
	if dst == avoid {
		return false
	}
	fn := src.Parent()
	if avoid == nil || !indexedByAnalyzer(fn, src) || dst.Parent() != fn || !indexedByAnalyzer(fn, dst) || avoid.Parent() != fn || !indexedByAnalyzer(fn, avoid) {
		return a.CanReach(src, dst)
	}
	g := a.graphOf(fn)
	key := [2]int{src.Index, avoid.Index}
	set, ok := g.avoiding[key]
	if !ok {
		set = make([]uint64, (len(fn.Blocks)+63)/64)
		queue := make([]*ssa.BasicBlock, 1, len(fn.Blocks))
		queue[0] = src
		for i := 0; i < len(queue); i++ {
			for _, succ := range queue[i].Succs {
				effort.Step()
				if succ == avoid || set[succ.Index/64]&(1<<(succ.Index%64)) != 0 {
					continue
				}
				set[succ.Index/64] |= 1 << (succ.Index % 64)
				queue = append(queue, succ)
			}
		}
		if g.avoiding == nil {
			g.avoiding = make(map[[2]int][]uint64)
		}
		g.avoiding[key] = set
	}
	return set[dst.Index/64]&(1<<(dst.Index%64)) != 0
}

// reachOutsideAnalyzer reports whether src reaches dst, by a breadth-first
// search through successor edges, for blocks the cache does not index.
func reachOutsideAnalyzer(src, dst *ssa.BasicBlock) bool {
	visited := make(map[*ssa.BasicBlock]bool)
	queue := []*ssa.BasicBlock{src}
	visited[src] = true

	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]

		for _, succ := range block.Succs {
			effort.Step()
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
