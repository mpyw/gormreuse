// Package pollution provides pollution state tracking for gormreuse.
//
// # Overview
//
// This package implements the "pollute model" for tracking *gorm.DB usage.
// It records usage sites during the first pass, then detects violations
// via CFG reachability in the second pass.
//
// # Two-Phase Detection
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│                      Detection Pipeline                                  │
//	│                                                                          │
//	│   Phase 1: RECORDING                  Phase 2: DETECTION                 │
//	│   ──────────────────                  ──────────────────                 │
//	│                                                                          │
//	│   q := db.Where("x")                  For each root with 2+ uses:        │
//	│   q.Find(nil) ──────▶ Record use #1      Check CFG reachability          │
//	│   q.Count(nil) ─────▶ Record use #2      If use#1 → use#2: VIOLATION     │
//	│                                                                          │
//	│   ┌───────────────────────────────────────────────────────────────┐     │
//	│   │  pollutingUses[root] = [{block1, pos1}, {block2, pos2}]       │     │
//	│   └───────────────────────────────────────────────────────────────┘     │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// # Pure vs Polluting Uses
//
//   - Polluting uses (ProcessBranch): Where, Find, Count, etc.
//     These "consume" the root and mark it polluted
//   - Pure uses (RecordPureUse): Session, Debug, WithContext
//     These CHECK for pollution but don't pollute themselves
//
// A pure use after a polluting use is still a violation (polluted root).
package pollution

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// Violation represents a detected reuse violation.
type Violation struct {
	Pos     token.Pos
	Message string
	Root    ssa.Value    // mutable root that caused the violation (for fix generation)
	AllUses []UsageInfo  // all uses of this root (for fix generation)
}

// UsageInfo tracks a single usage of a root (exported for fix generation).
type UsageInfo struct {
	Block *ssa.BasicBlock
	Pos   token.Pos
}

// Tracker tracks pollution state of mutable *gorm.DB roots.
//
// Design principle:
// - Each mutable root can only be used once (first branch)
// - Second branch from the same root is a violation
// - Variable assignment creates a NEW root (breaks pollution propagation)
// - Pure methods (Session, Debug) check pollution but don't pollute
type Tracker struct {
	// pollutingUses maps roots to non-pure method usage sites.
	// These uses "consume" the root and prevent further reuse.
	pollutingUses map[ssa.Value][]UsageInfo

	// pureUses maps roots to pure method usage sites.
	// These uses CHECK for pollution but don't pollute.
	pureUses map[ssa.Value][]UsageInfo

	// violations tracks detected violations.
	violations []Violation

	// cfgAnalyzer for reachability checks.
	cfgAnalyzer CFGAnalyzer

	// analyzedFn is the root function being analyzed.
	analyzedFn *ssa.Function
}

// CFGAnalyzer interface for control flow analysis.
type CFGAnalyzer interface {
	CanReach(src, dst *ssa.BasicBlock) bool
}

// New creates a new Tracker.
func New(cfgAnalyzer CFGAnalyzer, fn *ssa.Function) *Tracker {
	return &Tracker{
		pollutingUses: make(map[ssa.Value][]UsageInfo),
		pureUses:      make(map[ssa.Value][]UsageInfo),
		cfgAnalyzer:   cfgAnalyzer,
		analyzedFn:    fn,
	}
}

// ProcessBranch records a POLLUTING usage of a mutable root.
// Non-pure method calls that consume the root.
// Caller must ensure root is not nil.
func (t *Tracker) ProcessBranch(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) {
	t.pollutingUses[root] = append(t.pollutingUses[root], UsageInfo{Block: block, Pos: pos})
}

// RecordPureUse records a PURE usage (Session, Debug, etc).
// These uses check for pollution but don't pollute.
// Caller must ensure root is not nil.
func (t *Tracker) RecordPureUse(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) {
	t.pureUses[root] = append(t.pureUses[root], UsageInfo{Block: block, Pos: pos})
}

// isReachable checks if pollution can reach the target block.
func (t *Tracker) isReachable(pollutedBlock, targetBlock *ssa.BasicBlock) bool {
	if pollutedBlock == nil || targetBlock == nil {
		return false
	}

	// Cross-function (closure): conservative approach
	if pollutedBlock.Parent() != targetBlock.Parent() {
		return true
	}

	// Same function: use CFG reachability
	return t.cfgAnalyzer.CanReach(pollutedBlock, targetBlock)
}

// addViolation records a violation.
func (t *Tracker) addViolation(pos token.Pos) {
	t.violations = append(t.violations, Violation{
		Pos:     pos,
		Message: "*gorm.DB instance reused after chain method (use .Session(&gorm.Session{}) to make it safe)",
	})
}

// addViolationWithContext adds a violation with root and uses information for fix generation.
func (t *Tracker) addViolationWithContext(pos token.Pos, root ssa.Value, allUses []UsageInfo) {
	t.violations = append(t.violations, Violation{
		Pos:     pos,
		Message: "*gorm.DB instance reused after chain method (use .Session(&gorm.Session{}) to make it safe)",
		Root:    root,
		AllUses: allUses,
	})
}

// IsPolluted checks if a root has been polluted (for defer).
func (t *Tracker) IsPolluted(root ssa.Value) bool {
	uses := t.pollutingUses[root]
	return len(uses) > 0
}

// IsPollutedAt checks if a root has polluting usage that can reach the target block.
func (t *Tracker) IsPollutedAt(root ssa.Value, targetBlock *ssa.BasicBlock) bool {
	for _, use := range t.pollutingUses[root] {
		if t.isReachable(use.Block, targetBlock) {
			return true
		}
	}
	return false
}

// MarkPolluted records a polluting usage (for channel send, slice storage, etc).
// Caller must ensure root is not nil.
func (t *Tracker) MarkPolluted(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) {
	t.pollutingUses[root] = append(t.pollutingUses[root], UsageInfo{Block: block, Pos: pos})
}

// AddViolation explicitly adds a violation.
func (t *Tracker) AddViolation(pos token.Pos) {
	t.addViolation(pos)
}

// DetectViolations performs violation detection after all uses are recorded.
// For each root with multiple uses, check if an earlier use can reach a later one.
func (t *Tracker) DetectViolations() {
	// Check violations between polluting uses (non-pure methods)
	for root, uses := range t.pollutingUses {
		if len(uses) <= 1 {
			continue // Need at least 2 polluting uses for a violation
		}

		// Combine all uses (polluting + pure) for fix generation
		allUses := append([]UsageInfo{}, uses...)
		if pureUses, ok := t.pureUses[root]; ok {
			allUses = append(allUses, pureUses...)
		}

		// For each pair of uses, check if the earlier one can reach the later one
		for i, target := range uses {
			for j, src := range uses {
				if i == j {
					continue
				}

				// Only report if src is BEFORE target (earlier position)
				if src.Pos >= target.Pos {
					continue
				}

				// Different functions (closure): position order is sufficient
				if src.Block != nil && target.Block != nil &&
					src.Block.Parent() != target.Block.Parent() {
					t.addViolationWithContext(target.Pos, root, allUses)
					break
				}

				// Same function: check CFG reachability
				if t.isReachable(src.Block, target.Block) {
					t.addViolationWithContext(target.Pos, root, allUses)
					break
				}
			}
		}
	}

	// Check pure uses against polluting uses
	// A pure use after a polluting use is a violation
	for root, pureUses := range t.pureUses {
		pollutingUses := t.pollutingUses[root]
		if len(pollutingUses) == 0 {
			continue // No polluting uses for this root
		}

		// Combine all uses for fix generation
		allUses := append([]UsageInfo{}, pollutingUses...)
		allUses = append(allUses, pureUses...)

		for _, pureUse := range pureUses {
			for _, pollutingUse := range pollutingUses {
				// Only report if polluting is BEFORE pure (earlier position)
				if pollutingUse.Pos >= pureUse.Pos {
					continue
				}

				// Different functions (closure): position order is sufficient
				if pollutingUse.Block != nil && pureUse.Block != nil &&
					pollutingUse.Block.Parent() != pureUse.Block.Parent() {
					t.addViolationWithContext(pureUse.Pos, root, allUses)
					break
				}

				// Same function: check CFG reachability
				if t.isReachable(pollutingUse.Block, pureUse.Block) {
					t.addViolationWithContext(pureUse.Pos, root, allUses)
					break
				}
			}
		}
	}
}

// CollectViolations returns all detected violations.
func (t *Tracker) CollectViolations() []Violation {
	return t.violations
}

// IsPollutedAnywhere checks if root has any usage (for defer).
func (t *Tracker) IsPollutedAnywhere(root ssa.Value) bool {
	return t.IsPolluted(root)
}
