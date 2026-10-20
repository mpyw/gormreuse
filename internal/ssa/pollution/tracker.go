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
	"cmp"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// Violation represents a detected reuse violation.
type Violation struct {
	Pos     token.Pos
	Message string
	Root    ssa.Value   // mutable root that caused the violation (for fix generation)
	AllUses []UsageInfo // all uses of this root, filled in by CollectViolations (for fix generation)

	// HiddenUses is set when the root has uses that AllUses does not list:
	// deferred or spawned calls, reads through a Session/Debug copy or a
	// read-only argument, loop-carried reads, and branches through a merge
	// that may hold the root (#252). The fix cannot order them,
	// so it cannot prove that a Session is redundant (#202).
	HiddenUses bool

	// Call and Param locate the argument of a violation of the caller-side
	// immutable-param contract: the call, and the index of the parameter
	// the mutable value is passed to (callsite.Site.ArgParam). The fix
	// isolates that argument with a Session (#248). Call is nil for any
	// other violation.
	Call  ssa.CallInstruction
	Param int
}

// UsageInfo tracks a single usage of a root (exported for fix generation).
type UsageInfo struct {
	Block *ssa.BasicBlock
	Pos   token.Pos

	// Method and Call describe a use as the receiver of a *gorm.DB method
	// call, made directly or through a method value: the method's name and
	// the call's own position. Call differs from Pos for a use inside a
	// closure that is ordered at its single call site (#68). Both are zero
	// for any other use, such as an argument. The fix generator reads them to
	// find discarded statements (#199).
	Method string
	Call   token.Pos

	// Escapes is set when the use hands the root to other code, which may
	// change it in place: an argument of a function call, or a store into a
	// slice or map, or a channel send. A Session at the root would make that
	// code change a copy (#243).
	Escapes bool
}

// aliasUse is a use of a Session/Debug copy, with the position of the call
// that took the copy. When argument is set, it is a use of a chain that holds
// the root as a read-only argument, such as a subquery (#185), and copyPos is
// the call that took the argument.
type aliasUse struct {
	UsageInfo
	copyPos  token.Pos
	argument bool
}

// mayUse is a branch made through a value that may hold the root, but that
// the tracer resolves to another root first (see the mayUses field doc).
// ordered is set for a plain call, whose position orders it with the other
// uses; it is clear for a go or defer statement.
type mayUse struct {
	UsageInfo
	ordered bool
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
	// isPolluted/IsPollutedAt, which do consult this map.
	branchUses map[ssa.Value][]UsageInfo

	// aliasUses maps roots to uses of a Session/Debug copy that shares the
	// root's Statement (#174). Such a use reads the Statement as it is at that
	// moment, so any earlier in-place change to the root leaks into it: a
	// polluting use, and also an assignment use (q = q.Where(...) changes a
	// clone==0 Statement in place). Only a change after the copy was taken
	// counts: a change before it already makes the Session/Debug call itself a
	// second branch. Copies never change the root, so these uses do not pollute.
	// A chain that holds the root as a read-only argument reads it the same
	// way when it runs (#185).
	aliasUses map[ssa.Value][]aliasUse

	// loopCarriedUses maps roots to the calls in a loop that read the root's
	// value from the previous iteration: the call is its own root, as in
	// `for { r.db = r.db.Where(c) }` (#182). A polluting use of the root that
	// can reach such a call runs before it on the next iteration, whatever
	// their positions, so DetectViolations checks these by reachability alone.
	loopCarriedUses map[ssa.Value][]UsageInfo

	// mayUses maps roots to branches made through a merge that may hold the
	// root but resolves to another one first (#252): after
	// `q := b; if c { q = a }`, q.Find(nil) is a branch of b, and of a when c
	// holds. The uses of the other root are filed under b, so a later
	// a.Find(nil) would see no earlier branch of a. A may use is checked only
	// against the root's own uses, never against another may use: two values
	// that may each hold the root can be exclusive, as the variables of a swap
	// in a loop are.
	mayUses map[ssa.Value][]mayUse

	// violations tracks detected violations.
	violations []Violation

	// cfgAnalyzer for reachability checks.
	cfgAnalyzer CFGAnalyzer

	// fset renders root/first-branch positions in diagnostic messages. May be
	// nil (positions are then omitted).
	fset *token.FileSet

	// invocationSites maps a closure to the block of its one known invocation
	// in the enclosing function (a single plain call, IIFE included). A use in
	// such a closure is compared with uses elsewhere at that block, so the CFG
	// decides whether the two can both run (#179). Closures that are missing
	// here (several call sites, defer, go, passed or stored) stay conservative.
	invocationSites map[*ssa.Function]*ssa.BasicBlock
}

// CFGAnalyzer interface for control flow analysis.
type CFGAnalyzer interface {
	CanReach(src, dst *ssa.BasicBlock) bool
}

// New creates a new Tracker. fset is used to render source positions in reuse
// diagnostics and may be nil (positions are then omitted).
func New(cfgAnalyzer CFGAnalyzer, fset *token.FileSet) *Tracker {
	return &Tracker{
		pollutingUses:   make(map[ssa.Value][]UsageInfo),
		pureUses:        make(map[ssa.Value][]UsageInfo),
		assignmentUses:  make(map[ssa.Value][]UsageInfo),
		branchUses:      make(map[ssa.Value][]UsageInfo),
		aliasUses:       make(map[ssa.Value][]aliasUse),
		loopCarriedUses: make(map[ssa.Value][]UsageInfo),
		mayUses:         make(map[ssa.Value][]mayUse),
		cfgAnalyzer:     cfgAnalyzer,
		fset:            fset,
		invocationSites: make(map[*ssa.Function]*ssa.BasicBlock),
	}
}

// RecordInvocationSite records site as the block of the one known invocation
// of the closure fn (see the invocationSites field doc).
func (t *Tracker) RecordInvocationSite(fn *ssa.Function, site *ssa.BasicBlock) {
	t.invocationSites[fn] = site
}

// ProcessBranch records a POLLUTING usage of a mutable root.
// Non-pure method calls that consume the root.
// Caller must ensure root is not nil.
func (t *Tracker) ProcessBranch(root ssa.Value, use UsageInfo) {
	t.pollutingUses[root] = append(t.pollutingUses[root], use)
}

// RecordPureUse records a PURE usage (Session, Debug, etc).
// These uses check for pollution but don't pollute.
// Caller must ensure root is not nil.
func (t *Tracker) RecordPureUse(root ssa.Value, use UsageInfo) {
	t.pureUses[root] = append(t.pureUses[root], use)
}

// RecordAssignment records an ASSIGNMENT usage where a root is used to create a new root.
// This creates a new mutable root and doesn't count as pollution.
// Example: q = q.Where() creates new root from original q
// Caller must ensure root is not nil.
func (t *Tracker) RecordAssignment(root ssa.Value, use UsageInfo) {
	t.assignmentUses[root] = append(t.assignmentUses[root], use)
}

// RecordBranchUse records a deferred/goroutine polluting usage of a root.
// Recorded so a later defer/go can observe an earlier one; excluded from
// DetectViolations (see the branchUses field doc). Caller must ensure root is
// not nil.
func (t *Tracker) RecordBranchUse(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) {
	t.branchUses[root] = append(t.branchUses[root], UsageInfo{Block: block, Pos: pos})
}

// RecordAliasUse records a use of a Session/Debug copy that shares root's
// Statement, or of a chain that holds root as a read-only argument when
// argument is set (see the aliasUses field doc). copyPos is the position of
// the call that took the copy or the argument. Caller must ensure root is not
// nil.
func (t *Tracker) RecordAliasUse(root ssa.Value, copyPos token.Pos, argument bool, block *ssa.BasicBlock, pos token.Pos) {
	t.aliasUses[root] = append(t.aliasUses[root], aliasUse{Block: block, Pos: pos, copyPos: copyPos, argument: argument})
}

// RecordLoopCarriedUse records a call in a loop that reads root's value from
// the previous iteration (see the loopCarriedUses field doc). It does not
// pollute. Caller must ensure root is not nil.
func (t *Tracker) RecordLoopCarriedUse(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) {
	t.loopCarriedUses[root] = append(t.loopCarriedUses[root], UsageInfo{Block: block, Pos: pos})
}

// RecordMayBranch records a branch, at use, of a root that the branched value
// may hold besides the one it resolves to (see the mayUses field doc). ordered
// is set for a plain call, and clear for a go or defer statement, whose uses
// are not ordered by position. Caller must ensure root is not nil.
func (t *Tracker) RecordMayBranch(root ssa.Value, use UsageInfo, ordered bool) {
	t.mayUses[root] = append(t.mayUses[root], mayUse{UsageInfo: use, ordered: ordered})
}

// IsMayBranchedAt reports whether a branch recorded by RecordMayBranch can
// reach the target block.
func (t *Tracker) IsMayBranchedAt(root ssa.Value, targetBlock *ssa.BasicBlock) bool {
	return slices.ContainsFunc(t.mayUses[root], func(use mayUse) bool { return t.isReachable(use.Block, targetBlock) })
}

// IsMayBranchedAnywhere reports whether RecordMayBranch recorded any branch
// of root (for defer).
func (t *Tracker) IsMayBranchedAnywhere(root ssa.Value) bool {
	return len(t.mayUses[root]) > 0
}

// IsPollutedAt checks if a root has polluting usage that can reach the target block.
// Includes deferred/goroutine branch uses (see isPolluted).
func (t *Tracker) IsPollutedAt(root ssa.Value, targetBlock *ssa.BasicBlock) bool {
	reaches := func(use UsageInfo) bool { return t.isReachable(use.Block, targetBlock) }
	return slices.ContainsFunc(t.pollutingUses[root], reaches) || slices.ContainsFunc(t.branchUses[root], reaches)
}

// MarkPolluted records a polluting usage that hands the root to other code
// (a function argument, channel send, slice storage, etc). See
// UsageInfo.Escapes. Caller must ensure root is not nil.
func (t *Tracker) MarkPolluted(root ssa.Value, block *ssa.BasicBlock, pos token.Pos) {
	t.pollutingUses[root] = append(t.pollutingUses[root], UsageInfo{Block: block, Pos: pos, Escapes: true})
}

// AddContractViolation records a violation of the caller-side immutable-param
// contract (Phase 1b stage 2b): call passes a mutable *gorm.DB to parameter
// param of a //gormreuse:immutable-param function. It has a fixed message and
// no root. It still flows through the normal reporting path, so
// //gormreuse:ignore and position dedup apply.
func (t *Tracker) AddContractViolation(pos token.Pos, message string, call ssa.CallInstruction, param int) {
	t.violations = append(t.violations, Violation{Pos: pos, Message: message, Call: call, Param: param})
}

// AddViolationWithRoot adds a violation with root information for fix generation.
func (t *Tracker) AddViolationWithRoot(pos token.Pos, root ssa.Value) {
	t.addViolationWithContext(pos, root)
}

// DetectViolations performs violation detection after all uses are recorded.
// For each root with multiple uses, check if an earlier use can reach a later one.
//
// Roots are visited in a fixed order (see sortedRoots), never in map order: the
// order decides which root's violation comes first at a position shared by
// several roots, and so which root the diagnostic names and which one carries
// the suggested fix (#184).
func (t *Tracker) DetectViolations() {
	// Check violations between polluting uses (non-pure methods)
	for _, root := range sortedRoots(t, t.pollutingUses) {
		uses := t.pollutingUses[root]
		if len(uses) <= 1 {
			continue // Need at least 2 polluting uses for a violation
		}
		t.checkViolationsBetween(uses, uses, root)
	}

	// Check pure uses against polluting uses
	// A pure use after a polluting use is a violation
	for _, root := range sortedRoots(t, t.pureUses) {
		pollutingUses := t.pollutingUses[root]
		if len(pollutingUses) == 0 {
			continue
		}
		t.checkViolationsBetween(t.pureUses[root], pollutingUses, root)
	}

	// Check assignment uses against polluting uses
	// An assignment use after a polluting use is a violation (using polluted root)
	for _, root := range sortedRoots(t, t.assignmentUses) {
		pollutingUses := t.pollutingUses[root]
		if len(pollutingUses) == 0 {
			continue
		}
		t.checkViolationsBetween(t.assignmentUses[root], pollutingUses, root)
	}

	// Check the uses of a root against branches through a merge that may hold
	// it (#252), both ways: a use after such a branch, and such a branch after
	// a polluting use. Two such branches are never checked against each
	// other. go and defer uses are left to the inline checks, as for
	// branchUses.
	for _, root := range sortedRoots(t, t.mayUses) {
		var mayBranches []UsageInfo
		for _, use := range t.mayUses[root] {
			if use.ordered {
				mayBranches = append(mayBranches, use.UsageInfo)
			}
		}
		t.checkViolationsBetween(t.getAllUses(root), mayBranches, root)
		t.checkViolationsBetween(mayBranches, t.pollutingUses[root], root)
	}

	// Check uses of a Session/Debug copy against changes to the root it shares
	// a Statement with. These carry no suggested fix: the reuse-fix strategies
	// rewrite the root's branches, not the copy.
	//
	// A read-only argument is reported once per call that took it, at the
	// earliest use that sees the change: every call of a chain such as
	// x.Order("id").Find(nil) is a use of x, and one statement needs one
	// report.
	for _, root := range sortedRoots(t, t.aliasUses) {
		changes := slices.Concat(t.pollutingUses[root], t.assignmentUses[root])
		argumentReads := make(map[token.Pos]Violation)
		for _, use := range t.aliasUses[root] {
			change, ok := t.earlierReaching(changes, use.UsageInfo, use.copyPos)
			switch {
			case !ok:
			case !use.argument:
				t.violations = append(t.violations, Violation{Pos: use.Pos, Message: t.aliasMessage(root, change.Pos)})
			case argumentReads[use.copyPos].Pos == token.NoPos || use.Pos < argumentReads[use.copyPos].Pos:
				argumentReads[use.copyPos] = Violation{Pos: use.Pos, Message: t.argumentMessage(root, change.Pos)}
			}
		}
		for _, copyPos := range slices.Sorted(maps.Keys(argumentReads)) {
			t.violations = append(t.violations, argumentReads[copyPos])
		}
	}

	// Check loop-carried uses against polluting uses that can reach them.
	// Positions do not order them: the polluting use may come later in the
	// loop body and still run before the next iteration's call.
	for _, root := range sortedRoots(t, t.loopCarriedUses) {
		for _, target := range t.loopCarriedUses[root] {
			if slices.ContainsFunc(t.pollutingUses[root], func(src UsageInfo) bool { return t.isReachable(src.Block, target.Block) }) {
				t.addViolationWithContext(target.Pos, root)
			}
		}
	}
}

// earlierReaching returns a use in sources that comes after the position
// after and before target, and can reach target, under the same rules as
// checkViolationsBetween.
func (t *Tracker) earlierReaching(sources []UsageInfo, target UsageInfo, after token.Pos) (UsageInfo, bool) {
	for _, src := range sources {
		if src.Pos >= target.Pos || src.Pos <= after {
			continue
		}
		if t.isReachable(src.Block, target.Block) {
			return src, true
		}
	}
	return UsageInfo{}, false
}

// aliasMessage builds the diagnostic for a use of a Session/Debug copy after
// the root it shares a Statement with was changed at changePos.
func (t *Tracker) aliasMessage(root ssa.Value, changePos token.Pos) string {
	return "*gorm.DB reused: Session/Debug copy shares the Statement of a mutable root" +
		t.changeLocs(root, changePos) +
		", so the copy sees that change; end the root's chain with .Session(&gorm.Session{}) before copying it"
}

// changeLocs renders " (root at file:line, changed at file:line)" for the
// alias diagnostics, leaving out a position that is unknown.
func (t *Tracker) changeLocs(root ssa.Value, changePos token.Pos) string {
	var locs []string
	if root != nil && root.Pos().IsValid() {
		locs = append(locs, "root at "+t.loc(root.Pos()))
	}
	if changePos.IsValid() {
		locs = append(locs, "changed at "+t.loc(changePos))
	}
	if len(locs) == 0 || t.fset == nil {
		return ""
	}
	return " (" + strings.Join(locs, ", ") + ")"
}

// argumentMessage builds the diagnostic for a use of a chain that holds root
// as a read-only argument, after root was changed at changePos (#185).
func (t *Tracker) argumentMessage(root ssa.Value, changePos token.Pos) string {
	return "*gorm.DB reused: mutable root passed as a query argument is read when the query runs" +
		t.changeLocs(root, changePos) +
		", so the query sees that change; end the root's chain with .Session(&gorm.Session{}) before passing it"
}

// sortedRoots returns the roots of uses ordered by root position, then by
// earliest recorded use, then by SSA name, function, and instruction text. The
// later keys only break ties between roots without a source position (such as
// Phi nodes). It is generic so the aliasUses map sorts the same way.
//
// Each root's keys are computed once, and its names only when a tie needs
// them. Computing them in the comparison made n log n copies of the uses and
// of the names, which dominated a function with n loop-header Phi roots (#300).
func sortedRoots[U any](t *Tracker, uses map[ssa.Value][]U) []ssa.Value {
	keys := make([]rootOrder, 0, len(uses))
	for root := range uses {
		keys = append(keys, rootOrder{root: root, pos: root.Pos(), earliest: t.earliestUse(root)})
	}
	order := make([]*rootOrder, len(keys))
	for i := range keys {
		order[i] = &keys[i]
	}
	slices.SortFunc(order, func(a, b *rootOrder) int {
		if c := cmp.Or(cmp.Compare(a.pos, b.pos), cmp.Compare(a.earliest, b.earliest)); c != 0 {
			return c
		}
		a.name()
		b.name()
		return cmp.Or(
			strings.Compare(a.ssaName, b.ssaName),
			strings.Compare(a.funcName, b.funcName),
			strings.Compare(a.text, b.text),
		)
	})
	roots := make([]ssa.Value, len(order))
	for i, k := range order {
		roots[i] = k.root
	}
	return roots
}

// rootOrder is a root and the keys sortedRoots orders it by. The names are
// empty until name fills them.
type rootOrder struct {
	root                    ssa.Value
	pos, earliest           token.Pos
	named                   bool
	ssaName, funcName, text string
}

// name fills the names of the root once.
func (k *rootOrder) name() {
	if !k.named {
		k.named = true
		k.ssaName, k.funcName, k.text = k.root.Name(), funcName(k.root), k.root.String()
	}
}

// earliestUse returns the smallest recorded use position of root.
func (t *Tracker) earliestUse(root ssa.Value) token.Pos {
	best := token.NoPos
	for _, u := range t.getAllUses(root) {
		if best == token.NoPos || u.Pos < best {
			best = u.Pos
		}
	}
	return best
}

// funcName names the function that defines v, or "" for a value outside any
// function (a global or a constant).
func funcName(v ssa.Value) string {
	if fn := v.Parent(); fn != nil {
		return fn.String()
	}
	return ""
}

// CollectViolations returns all detected violations, each with every use of
// its root. The uses are read here, after tracking has finished, and not when
// the violation is added: a loop violation is added while its use is tracked,
// before the uses after it in the loop body are recorded. A fix built from
// that snapshot never sees them (#199).
func (t *Tracker) CollectViolations() []Violation {
	for i := range t.violations {
		if root := t.violations[i].Root; root != nil {
			t.violations[i].AllUses = t.getAllUses(root)
			t.violations[i].HiddenUses = len(t.branchUses[root]) > 0 || len(t.aliasUses[root]) > 0 || len(t.loopCarriedUses[root]) > 0 || len(t.mayUses[root]) > 0
		}
	}
	return t.violations
}

// IsPollutedAnywhere checks if root has any usage (for defer).
func (t *Tracker) IsPollutedAnywhere(root ssa.Value) bool {
	return t.isPolluted(root)
}

// isReachable checks if pollution can reach the target block.
func (t *Tracker) isReachable(pollutedBlock, targetBlock *ssa.BasicBlock) bool {
	if pollutedBlock == nil || targetBlock == nil {
		return false
	}

	// Same function: use CFG reachability
	if pollutedBlock.Parent() == targetBlock.Parent() {
		return t.cfgAnalyzer.CanReach(pollutedBlock, targetBlock)
	}

	// Cross-function (closure): compare the two at the blocks where their
	// closures are invoked in the innermost function holding both (#179). Two
	// sites in the same block, or no such function (a closure with no known
	// invocation site on the way), leave the order to the positions.
	for _, src := range t.invocationChain(pollutedBlock) {
		for _, dst := range t.invocationChain(targetBlock) {
			if src.Parent() == dst.Parent() {
				return t.cfgAnalyzer.CanReach(src, dst)
			}
		}
	}
	return true
}

// invocationChain returns block, then the invocation site of its function,
// then that site's invocation site, and so on while each closure has a known
// one. Each block lies in a function enclosing the previous one.
func (t *Tracker) invocationChain(block *ssa.BasicBlock) []*ssa.BasicBlock {
	chain := []*ssa.BasicBlock{block}
	for site, ok := t.invocationSites[block.Parent()]; ok; site, ok = t.invocationSites[site.Parent()] {
		chain = append(chain, site)
	}
	return chain
}

// addViolationWithContext adds a violation with root information for fix
// generation. CollectViolations adds the root's uses.
func (t *Tracker) addViolationWithContext(pos token.Pos, root ssa.Value) {
	t.violations = append(t.violations, Violation{
		Pos:     pos,
		Message: t.reuseMessage(root),
		Root:    root,
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
	// Deliberately adjusted: this text sits next to the driver-printed position.
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
	for _, u := range t.mayUses[root] {
		if u.ordered && u.Pos.IsValid() && (!best.IsValid() || u.Pos < best) {
			best = u.Pos
		}
	}
	return best
}

// isPolluted checks if a root has been polluted (for defer).
// Includes deferred/goroutine branch uses so multiple defers/goroutines that
// reuse the same root (with no direct use) are detected.
func (t *Tracker) isPolluted(root ssa.Value) bool {
	return len(t.pollutingUses[root]) > 0 || len(t.branchUses[root]) > 0
}

// getAllUses returns all uses (pure + polluting + assignment) for a root.
func (t *Tracker) getAllUses(root ssa.Value) []UsageInfo {
	return slices.Concat(t.pureUses[root], t.pollutingUses[root], t.assignmentUses[root])
}

// checkViolationsBetween checks if any source use can reach any target use.
// Returns positions of target uses that are reachable from a source use.
func (t *Tracker) checkViolationsBetween(targets, sources []UsageInfo, root ssa.Value) {
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

			// Check CFG reachability (across closures, at their invocation sites)
			if t.isReachable(src.Block, target.Block) {
				t.addViolationWithContext(target.Pos, root)
				break
			}
		}
	}
}
