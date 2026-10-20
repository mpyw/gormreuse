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
	"go/ast"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/effort"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
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

	// Direct is set when the use's value is the root itself, or a chain of
	// *gorm.DB method calls on it, with no merge, variable, field or closure
	// in between, and the call names its function statically. Such a use
	// sees the root of the latest run of the root's definition. Two direct
	// uses of a root that a call defines reuse it only when one runs after
	// the other with no run of that call in between (#375). Direct is only
	// meaningful for the first root of the use's value.
	Direct bool
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
	// IsPollutedAt and IsPollutedAround, which do consult this map.
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
	CanReachAvoiding(src, dst, avoid *ssa.BasicBlock) bool
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
func (t *Tracker) RecordBranchUse(root ssa.Value, use UsageInfo) {
	t.branchUses[root] = append(t.branchUses[root], use)
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
// reach target, a use of root (for go).
func (t *Tracker) IsMayBranchedAt(root ssa.Value, target UsageInfo) bool {
	return slices.ContainsFunc(t.mayUses[root], func(use mayUse) bool { return t.reaches(root, use.UsageInfo, target) })
}

// IsMayBranchedAround reports whether a branch recorded by RecordMayBranch
// may run in the same run of the function as target, a use of root, before
// or after it (for defer).
func (t *Tracker) IsMayBranchedAround(root ssa.Value, target UsageInfo) bool {
	return slices.ContainsFunc(t.mayUses[root], func(use mayUse) bool { return t.runsWith(root, use.UsageInfo, target) })
}

// IsPollutedAt checks if a root has polluting usage that can reach target, a
// use of root (for go). Includes deferred/goroutine branch uses (see the
// branchUses field doc).
func (t *Tracker) IsPollutedAt(root ssa.Value, target UsageInfo) bool {
	reaches := func(use UsageInfo) bool { return t.reaches(root, use, target) }
	return slices.ContainsFunc(t.pollutingUses[root], reaches) || slices.ContainsFunc(t.branchUses[root], reaches)
}

// IsPollutedAround reports whether a polluting use of root, deferred and
// spawned ones included, may run in the same run of the function as target,
// before or after it (for defer). A deferred call runs when the function
// returns, so any use that may run in that run of the function comes before
// it. A use on a branch exclusive with the defer statement does not (#375).
func (t *Tracker) IsPollutedAround(root ssa.Value, target UsageInfo) bool {
	with := func(use UsageInfo) bool { return t.runsWith(root, use, target) }
	return slices.ContainsFunc(t.pollutingUses[root], with) || slices.ContainsFunc(t.branchUses[root], with)
}

// runsWith reports whether a and b, two uses of root, may both run in one run
// of their function, in either order.
func (t *Tracker) runsWith(root ssa.Value, a, b UsageInfo) bool {
	return t.reaches(root, a, b) || t.reaches(root, b, a)
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
		// A loop-carried call is its own root, so the path to it always runs
		// the call again: reaches would never hold.
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
	if root != nil && rootPos(root).IsValid() {
		locs = append(locs, "root at "+t.loc(rootPos(root)))
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
// that snapshot never sees them (#199). The violations of one root share its
// uses, which nothing changes, so a root reused n times costs one list, not
// n (#350).
func (t *Tracker) CollectViolations() []Violation {
	shared := make(map[ssa.Value][]UsageInfo)
	for i := range t.violations {
		if root := t.violations[i].Root; root != nil {
			uses, ok := shared[root]
			if !ok {
				uses = t.getAllUses(root)
				uses = slices.Clip(uses)
				shared[root] = uses
			}
			t.violations[i].AllUses = t.violationUses(t.violations[i], uses)
			t.violations[i].HiddenUses = len(t.branchUses[root]) > 0 || len(t.aliasUses[root]) > 0 || len(t.loopCarriedUses[root]) > 0 || len(t.mayUses[root]) > 0
		}
	}
	return t.violations
}

// violationUses returns the uses of v's root that its fix sees: the root's
// own, and a branch through a merge that may hold the root at v's position.
// The latter is the reported use when the branch reuses the root on every
// loop iteration, so without it the fix of a root whose only use is such a
// branch had no use to give a Session to (#306). A root used only in go and
// defer statements, as in `go q.Count(nil); go q.Find(nil)`, has only its
// branch uses, so the fix sees those (#309). uses are the root's own, shared
// by its violations.
func (t *Tracker) violationUses(v Violation, uses []UsageInfo) []UsageInfo {
	for _, use := range t.mayUses[v.Root] {
		if use.Pos == v.Pos && !slices.ContainsFunc(uses, func(u UsageInfo) bool { return u.Pos == use.Pos }) {
			uses = append(uses, use.UsageInfo) // a full slice: appending copies it
		}
	}
	if len(uses) == 0 {
		uses = slices.Clone(t.branchUses[v.Root])
	}
	return uses
}

// reaches reports whether src, a use of root, can reach the use dst with the
// same root. That is so when isReachable holds, except for two direct uses of
// a root that a call defines in their own function (UsageInfo.Direct). Each
// run of that call defines a new root, so the two uses share one only on a
// path, from either use to the other, that does not run the call again. In
// `for … { q := s.Where(…); if c { q.Delete(…) } else { q.Update(…) } }` the
// only path from one use to the other is through the back-edge, which
// defines q again (#375). A root that a merge, a variable or a closure holds
// may be one of an earlier run, so any path counts for it. A root that is
// not a call, such as a Phi, a parameter or a loaded field, is the same
// value on each path.
//
// The call is taken to return a new value on each run, as the loop rules
// take it (cfg.Repeats). A call that returns the same mutable *gorm.DB each
// time, such as a helper that hands out one shared handle, is missed here too.
func (t *Tracker) reaches(root ssa.Value, src, dst UsageInfo) bool {
	if !t.isReachable(src.Block, dst.Block) {
		return false
	}
	def, ok := root.(*ssa.Call)
	if !ok || !src.Direct || !dst.Direct || def.Block() == nil {
		return true
	}
	fn := def.Parent()
	if src.Block.Parent() != fn || dst.Block.Parent() != fn {
		return true
	}
	return t.cfgAnalyzer.CanReachAvoiding(src.Block, dst.Block, def.Block()) ||
		t.cfgAnalyzer.CanReachAvoiding(dst.Block, src.Block, def.Block())
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
	if root != nil && rootPos(root).IsValid() {
		locs = append(locs, "root at "+t.loc(rootPos(root)))
	}
	if fb := t.firstBranchPos(root); fb.IsValid() {
		locs = append(locs, "first branch at "+t.loc(fb))
	}
	if len(locs) > 0 {
		msg += " (" + strings.Join(locs, ", ") + ")"
	}

	return msg + reuseSessionAdvice
}

// reuseSessionAdvice ends a reuse diagnostic. reuseStateAdvice replaces it
// for a root whose Statement may carry state that a Session drops (#344).
const (
	reuseSessionAdvice = "; make the root immutable with .Session(&gorm.Session{})"
	reuseStateAdvice   = "; a Session would drop its Attrs, Assign or InstanceSet, so build each branch from a fresh chain instead"
)

// AdviseWithoutSession rewrites the advice of v, a reuse violation, for a
// root that a Session cannot make immutable without dropping state its
// Statement carries, such as Assign (#344).
func (v *Violation) AdviseWithoutSession() {
	if msg, ok := strings.CutSuffix(v.Message, reuseSessionAdvice); ok {
		v.Message = msg + reuseStateAdvice
	}
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

// rootPos returns the position that a report names for root. A parameter of
// the yield function of a range-over-func loop is a range variable, but its
// own position is in the declaration of the iterator's yield type, such as
// iter.Seq in the standard library (#378). Its position is that of the range
// variable, or the for keyword when the loop does not name it.
func rootPos(root ssa.Value) token.Pos {
	param, ok := root.(*ssa.Parameter)
	if !ok || !cfg.IsRangeFuncLoopBody(param.Parent()) {
		return root.Pos()
	}
	// go/ssa gives the yield function its range statement as syntax, and a
	// parameter for the key and one for the value at most.
	rng := param.Parent().Syntax().(*ast.RangeStmt)
	pos := rng.For
	if v := []ast.Expr{rng.Key, rng.Value}[slices.Index(param.Parent().Params, param)]; v != nil {
		pos = v.Pos()
	}
	return pos
}

// firstBranchPos returns the earliest polluting use of root (its first branch),
// which precedes the second branch that triggered the violation. A root with
// no such use, only branches in go and defer statements, has its first
// branch among those, recorded before the one reported (#309). They are not
// ordered by position, so they count only then.
func (t *Tracker) firstBranchPos(root ssa.Value) token.Pos {
	best := token.NoPos
	earliest := func(u UsageInfo) {
		if u.Pos.IsValid() && (!best.IsValid() || u.Pos < best) {
			best = u.Pos
		}
	}
	for _, u := range t.pollutingUses[root] {
		earliest(u)
	}
	for _, u := range t.mayUses[root] {
		if u.ordered {
			earliest(u.UsageInfo)
		}
	}
	if best.IsValid() {
		return best
	}
	for _, u := range t.branchUses[root] {
		earliest(u)
	}
	for _, u := range t.mayUses[root] {
		earliest(u.UsageInfo)
	}
	return best
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
			effort.Step()
			// Skip self-comparison for same-list checks
			if src.Pos == target.Pos && src.Block == target.Block {
				continue
			}

			// Only report if src is BEFORE target (earlier position)
			if src.Pos >= target.Pos {
				continue
			}

			// Check CFG reachability (across closures, at their invocation
			// sites), on a path that keeps the root of src (#375)
			if t.reaches(root, src, target) {
				t.addViolationWithContext(target.Pos, root)
				break
			}
		}
	}
}
