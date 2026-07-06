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
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// Violation represents a detected reuse violation.
type Violation struct {
	Pos     token.Pos
	Message string
	Root    ssa.Value   // mutable root that caused the violation (for fix generation)
	AllUses []UsageInfo // all uses of this root (for fix generation)
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

	// assignmentUses maps roots to assignment sites where they're used to create new roots.
	// These uses create new roots and don't count as pollution.
	// Example: q = q.Where() creates new root from original q
	assignmentUses map[ssa.Value][]UsageInfo

	// branchUses maps roots to deferred/goroutine usage sites. Like polluting
	// uses they consume the root, but they are recorded so that a LATER defer/go
	// can see an EARLIER one (defer q.Find(); defer q.Count() with no direct use).
	// They are intentionally NOT scanned by DetectViolations: defer/go run at a
	// different time than their textual position, so position-ordered detection
	// would misfire. Violations among them are reported inline at record time via
	// IsPolluted/IsPollutedAt, which do consult this map.
	branchUses map[ssa.Value][]UsageInfo

	// violations tracks detected violations.
	violations []Violation

	// cfgAnalyzer for reachability checks.
	cfgAnalyzer CFGAnalyzer

	// fset renders root/first-branch positions in diagnostic messages. May be
	// nil (positions are then omitted).
	fset *token.FileSet
}

// CFGAnalyzer interface for control flow analysis.
type CFGAnalyzer interface {
	CanReach(src, dst *ssa.BasicBlock) bool
}

// New creates a new Tracker. fset is used to render source positions in reuse
// diagnostics and may be nil (positions are then omitted).
func New(cfgAnalyzer CFGAnalyzer, fset *token.FileSet) *Tracker {
	return &Tracker{
		pollutingUses:  make(map[ssa.Value][]UsageInfo),
		pureUses:       make(map[ssa.Value][]UsageInfo),
		assignmentUses: make(map[ssa.Value][]UsageInfo),
		branchUses:     make(map[ssa.Value][]UsageInfo),
		cfgAnalyzer:    cfgAnalyzer,
		fset:           fset,
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

// RecordAssignment records an ASSIGNMENT usage where a root is used to create a new root.
// This creates a new mutable root and doesn't count as pollution.
// Example: q = q.Where() creates new root from original q
// Caller must ensure root is not nil.
func (t *Tracker) RecordAssignment(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) {
	t.assignmentUses[root] = append(t.assignmentUses[root], UsageInfo{Block: block, Pos: pos})
}

// RecordBranchUse records a deferred/goroutine polluting usage of a root.
// Recorded so a later defer/go can observe an earlier one; excluded from
// DetectViolations (see the branchUses field doc). Caller must ensure root is
// not nil.
func (t *Tracker) RecordBranchUse(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) {
	t.branchUses[root] = append(t.branchUses[root], UsageInfo{Block: block, Pos: pos})
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

// addViolationWithContext adds a violation with root and uses information for fix generation.
func (t *Tracker) addViolationWithContext(pos token.Pos, root ssa.Value, allUses []UsageInfo) {
	t.violations = append(t.violations, Violation{
		Pos:     pos,
		Message: t.reuseMessage(root),
		Root:    root,
		AllUses: allUses,
	})
}

// reuseMessage builds the reuse diagnostic. It names the mutable root and its
// first branch (when their positions are known) so the report points the user
// at all three sites — root, first branch, and the offending second branch (the
// diagnostic's own position) — not just the last one (#76).
func (t *Tracker) reuseMessage(root ssa.Value) string {
	msg := "*gorm.DB reused: second branch from mutable root"

	var locs []string
	if root != nil && root.Pos().IsValid() {
		locs = append(locs, "root at "+t.loc(root.Pos()))
	}
	if fb := t.firstBranchPos(root); fb.IsValid() {
		locs = append(locs, "first branch at "+t.loc(fb))
	}
	if len(locs) > 0 {
		msg += " (" + strings.Join(locs, ", ") + ")"
	}

	return msg + "; make the root immutable with .Session(&gorm.Session{})"
}

// loc renders pos as "file.go:line" (base name only — the file is almost always
// the one being reported on, and absolute paths would be noise).
func (t *Tracker) loc(pos token.Pos) string {
	if t.fset == nil {
		return ""
	}
	p := t.fset.Position(pos)
	return filepath.Base(p.Filename) + ":" + strconv.Itoa(p.Line)
}

// firstBranchPos returns the earliest polluting use of root (its first branch),
// which precedes the second branch that triggered the violation.
func (t *Tracker) firstBranchPos(root ssa.Value) token.Pos {
	best := token.NoPos
	for _, u := range t.pollutingUses[root] {
		if u.Pos.IsValid() && (!best.IsValid() || u.Pos < best) {
			best = u.Pos
		}
	}
	return best
}

// IsPolluted checks if a root has been polluted (for defer).
// Includes deferred/goroutine branch uses so multiple defers/goroutines that
// reuse the same root (with no direct use) are detected.
func (t *Tracker) IsPolluted(root ssa.Value) bool {
	return len(t.pollutingUses[root]) > 0 || len(t.branchUses[root]) > 0
}

// IsPollutedAt checks if a root has polluting usage that can reach the target block.
// Includes deferred/goroutine branch uses (see IsPolluted).
func (t *Tracker) IsPollutedAt(root ssa.Value, targetBlock *ssa.BasicBlock) bool {
	for _, use := range t.pollutingUses[root] {
		if t.isReachable(use.Block, targetBlock) {
			return true
		}
	}
	for _, use := range t.branchUses[root] {
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

// AddMessageViolation records a violation with a fixed message and no root, so it
// carries no suggested fix. Used for contract violations that are not root-reuse
// violations — e.g. passing a mutable *gorm.DB to a //gormreuse:immutable-param
// parameter (Phase 1b stage 2b). It still flows through the normal reporting path,
// so //gormreuse:ignore and position dedup apply.
func (t *Tracker) AddMessageViolation(pos token.Pos, message string) {
	t.violations = append(t.violations, Violation{Pos: pos, Message: message})
}

// AddViolationWithRoot adds a violation with root information for fix generation.
func (t *Tracker) AddViolationWithRoot(pos token.Pos, root ssa.Value) {
	allUses := t.getAllUses(root)
	t.addViolationWithContext(pos, root, allUses)
}

// getAllUses returns all uses (pure + polluting + assignment) for a root.
func (t *Tracker) getAllUses(root ssa.Value) []UsageInfo {
	var allUses []UsageInfo
	allUses = append(allUses, t.pureUses[root]...)
	allUses = append(allUses, t.pollutingUses[root]...)
	allUses = append(allUses, t.assignmentUses[root]...)
	return allUses
}

// checkViolationsBetween checks if any source use can reach any target use.
// Returns positions of target uses that are reachable from a source use.
func (t *Tracker) checkViolationsBetween(targets, sources []UsageInfo, root ssa.Value, allUses []UsageInfo) {
	for _, target := range targets {
		for _, src := range sources {
			// Skip self-comparison for same-list checks
			if src.Pos == target.Pos && src.Block == target.Block {
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

// DetectViolations performs violation detection after all uses are recorded.
// For each root with multiple uses, check if an earlier use can reach a later one.
func (t *Tracker) DetectViolations() {
	// Check violations between polluting uses (non-pure methods)
	for root, uses := range t.pollutingUses {
		if len(uses) <= 1 {
			continue // Need at least 2 polluting uses for a violation
		}
		allUses := t.getAllUses(root)
		t.checkViolationsBetween(uses, uses, root, allUses)
	}

	// Check pure uses against polluting uses
	// A pure use after a polluting use is a violation
	for root, pureUses := range t.pureUses {
		pollutingUses := t.pollutingUses[root]
		if len(pollutingUses) == 0 {
			continue
		}
		allUses := t.getAllUses(root)
		t.checkViolationsBetween(pureUses, pollutingUses, root, allUses)
	}

	// Check assignment uses against polluting uses
	// An assignment use after a polluting use is a violation (using polluted root)
	for root, assignmentUses := range t.assignmentUses {
		pollutingUses := t.pollutingUses[root]
		if len(pollutingUses) == 0 {
			continue
		}
		allUses := t.getAllUses(root)
		t.checkViolationsBetween(assignmentUses, pollutingUses, root, allUses)
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
