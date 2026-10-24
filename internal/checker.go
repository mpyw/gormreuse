package internal

import (
	"cmp"
	"go/token"
	"slices"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/fix"
	ssautil "github.com/mpyw/gormreuse/internal/ssa"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// checker wraps SSA analysis with ignore directive handling.
//
// It ensures:
//   - Violations at the same position are only reported once
//   - Ignore directives suppress violations
//   - Violations are reported through the analysis.Pass
type checker struct {
	pass                *analysis.Pass           // For reporting diagnostics
	ignores             *directive.Ignores       // Line, function and file ignore directives
	rootTracer          *tracer.RootTracer       // Tracer of the pass, shared by every function (#325)
	needsImmutableParam func(*ssa.Function) bool // immutable-param fns that branch a param (2b caller check)
	reported            map[reportKey]bool       // Deduplication of reports
	ignoredRoots        map[ssa.Value]bool       // Roots with a diagnostic an ignore suppresses, in any function (#346)
	pureReports         checkerPureReports       // Reports that only a pure method's receiver branch explains (#404)
	fixGen              *fix.Generator           // Cached fix generator for all violations
	cfgs                *cfg.Analyzer            // CFG cache of the pass, shared with fixGen (#299)
}

// newChecker creates the checker of a package, which analyzes and reports
// every function.
// The reported map is shared across all functions to deduplicate violations
// across parent functions and their closures.
// The fixGen is shared to avoid recreating the generator for each violation.
//
//declscope:shared // run.go makes a checker per package
func newChecker(pass *analysis.Pass, ignores *directive.Ignores, rootTracer *tracer.RootTracer, needsImmutableParam func(*ssa.Function) bool, reported map[reportKey]bool, fixGen *fix.Generator, cfgs *cfg.Analyzer) *checker {
	return &checker{
		pass:                pass,
		ignores:             ignores,
		rootTracer:          rootTracer,
		needsImmutableParam: needsImmutableParam,
		reported:            reported,
		ignoredRoots:        make(map[ssa.Value]bool),
		fixGen:              fixGen,
		cfgs:                cfgs,
	}
}

// analyze runs SSA analysis on a single function and returns its
// violations. It records the roots of the violations an ignore directive
// suppresses: such a root gets no fix in any function (see report).
//
//declscope:shared // run.go analyzes every function with it before reporting
func (c *checker) analyze(fn *ssa.Function) []pollution.Violation {
	violations := ssautil.NewAnalyzer(fn, c.rootTracer, c.needsImmutableParam, c.cfgs).Analyze()
	for _, v := range violations {
		// A report that only a pure method's receiver branch explains (#404)
		// was not a report before, so ignoring it takes no fix away.
		if v.Root != nil && !v.FromPureReceiver && c.ignores.Covers(v.Pos) {
			c.ignoredRoots[v.Root] = true
		}
		c.pureReports.add(v, c.ignores.Covers(v.Pos))
	}
	return violations
}

// report reports the violations of a function that analyze returned.
//
//declscope:shared // run.go reports every function with it
func (c *checker) report(violations []pollution.Violation) {
	// Every diagnostic of a root carries the root's fix, not only the first:
	// the first may be already reported, and an editor offers a quick fix
	// only on the diagnostic that carries it (#246). Each fix is complete on
	// its own (#200), so the copies are identical, and the drivers apply an
	// edit that several fixes share once. A root's fix is generated once, for
	// the first diagnostic of the root that is reported. It depends only on
	// the root and its uses.
	//
	// A root with a diagnostic that an ignore directive suppresses gets no
	// fix (#312). The ignore says that reuse is meant, and the fix would
	// change it too: it isolates the root or reassigns the ignored statement,
	// so the next run would report the ignore as unused. The diagnostic may
	// belong to another function, such as a stored closure that uses the
	// root, so analyze collects the roots of every function first (#346).
	fixes := make(map[ssa.Value][]analysis.SuggestedFix)
	fixOf := func(v pollution.Violation) []analysis.SuggestedFix {
		if c.ignoredRoots[v.Root] {
			return nil
		}
		f, ok := fixes[v.Root]
		if !ok {
			// The fix of a root with a report that only a pure method's
			// receiver branch explains is dropped, after Generate decided
			// it as before #404, so the advice of its diagnostics is too.
			f = c.fixGen.Generate(v)
			if c.pureReports.roots()[v.Root] {
				f = nil
			}
			fixes[v.Root] = f
		}
		return f
	}

	// Group violations by position. Several roots can be reused at one position
	// (use2(q, r)), and only one diagnostic is reported there, so it carries the
	// fix of every root in the group. Otherwise only the first root would get a
	// fix (#184). A violation with no root, such as a read-only argument that
	// sees a change, is another bug, and is reported on its own (see
	// reportKey). One such diagnostic stands for every violation in the group
	// with its message, such as each mutable argument of f(q, r) passed to an
	// immutable-param function, so it carries all their fixes (#248).
	for _, group := range reportGroupByPos(violations) {
		var reuse, fixFor []pollution.Violation
		for _, v := range group {
			if v.Root == nil {
				c.reportViolation(v, func() []analysis.SuggestedFix {
					var fixes []analysis.SuggestedFix
					for _, same := range group {
						if same.Root == nil && same.Message == v.Message {
							fixes = append(fixes, c.fixGen.GenerateIsolation(same)...)
						}
					}
					return fixes
				})
				continue
			}
			reuse = append(reuse, v)
			// A root whose only report here needs a pure method's receiver
			// branch (#404) had no report here before, so this diagnostic
			// does not carry its fix.
			if !v.FromPureReceiver && !slices.ContainsFunc(fixFor, func(f pollution.Violation) bool { return f.Root == v.Root }) {
				fixFor = append(fixFor, v)
			}
		}
		if len(reuse) > 0 {
			// A report that holds only because of a pure method's receiver
			// branch (#404) comes after one that holds without it, so the
			// diagnostic keeps the text it had before #404.
			slices.SortStableFunc(reuse, func(a, b pollution.Violation) int {
				return cmp.Compare(checkerBoolInt(a.FromPureReceiver), checkerBoolInt(b.FromPureReceiver))
			})
			c.reportViolation(reuse[0], func() []analysis.SuggestedFix {
				var fixes []analysis.SuggestedFix
				for _, fv := range fixFor {
					fixes = append(fixes, fixOf(fv)...)
				}
				// The advice of the diagnostic depends on its root's fix (see
				// reportViolation). A report that only a pure method's receiver
				// branch explains (#404) carries no fix, so its root's fix is
				// decided from a report that holds without the branch, as main
				// decides it. Only a root with no such report is decided from
				// this one. Neither is kept as the root's fix.
				if head := reuse[0]; head.FromPureReceiver && !c.ignoredRoots[head.Root] {
					c.pureReports.decideAdvice(head, c.fixGen.Generate)
				}
				return fixes
			})
		}
	}
}

// reportViolation reports v with the suggested fixes that fixes returns. The
// fixes are merged into one, because drivers apply only the first suggested
// fix of a diagnostic. fixes is called only when v is reported.
func (c *checker) reportViolation(v pollution.Violation, fixes func() []analysis.SuggestedFix) {
	pos := v.Pos

	// Deduplicate: same position may be reached multiple times
	if c.reported[reportKeyOf(v)] {
		return
	}
	c.reported[reportKeyOf(v)] = true

	if c.ignores.Ignore(pos) {
		return // Suppressed by ignore directive
	}

	// Each diagnostic carries every edit its roots need, even one that another
	// diagnostic carries too, such as a shared Phi edge's Session or the gorm
	// import. Applying one diagnostic's fix alone must compile (#200). Drivers
	// that apply several fixes, such as -fix, merge identical edits.
	suggestedFixes := mergeReportFixes(fixes())

	// A Session at the root would let a later query run past an earlier
	// query's error, so the fix withheld it, and the message does not advise
	// it either (#372). The fixes above decided it for the root.
	if v.Root != nil && c.fixGen.SessionChangesErrors(v.Root) {
		v.AdviseCheckingErrors()
	}

	// Report with diagnostic
	c.pass.Report(analysis.Diagnostic{
		Pos:            pos,
		Message:        v.Message,
		SuggestedFixes: suggestedFixes,
	})
}

// checkerBoolInt returns 1 for true and 0 for false, to sort by a flag.
func checkerBoolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkerPureReports collects, over every function of the package, the
// reports that hold only because of a pure method's receiver branch (#404).
// A root gets no fix when one of them is shown. A reuse report is shown
// when no other reuse report is at its position: the checker reports one
// diagnostic per position, and puts first a report that holds without the
// branch. A Session/Debug copy report is shown on its own.
type checkerPureReports struct {
	other  map[token.Pos]bool
	pure   []pollution.Violation
	marked map[ssa.Value]bool
	// firstOther is the first report of each root that holds without a
	// pure method's receiver branch.
	firstOther map[ssa.Value]pollution.Violation
	// decided holds the roots whose advice decideAdvice decided.
	decided map[ssa.Value]bool
}

// decideAdvice decides the advice of v's root, once per root, for a shown
// report that only a pure method's receiver branch explains: generate runs
// on the root's first report that holds without the branch, as main runs it,
// or on v when the root has none. The fix it returns is not kept.
func (r *checkerPureReports) decideAdvice(v pollution.Violation, generate func(pollution.Violation) []analysis.SuggestedFix) {
	if r.decided[v.Root] {
		return
	}
	if r.decided == nil {
		r.decided = make(map[ssa.Value]bool)
	}
	r.decided[v.Root] = true
	if other, ok := r.firstOther[v.Root]; ok {
		v = other
	}
	generate(v)
}

// add records v, a violation of a function. ignored says that an ignore
// directive suppresses its position, so it is not shown.
func (r *checkerPureReports) add(v pollution.Violation, ignored bool) {
	switch {
	case v.Root != nil && !v.FromPureReceiver:
		if r.other == nil {
			r.other = make(map[token.Pos]bool)
			r.firstOther = make(map[ssa.Value]pollution.Violation)
		}
		r.other[v.Pos] = true
		if _, ok := r.firstOther[v.Root]; !ok {
			r.firstOther[v.Root] = v
		}
	case ignored:
	case v.Root != nil, v.PureReceiverRoot != nil:
		r.pure = append(r.pure, v)
	}
}

// roots returns the roots with a shown report. Every function is analyzed
// before the first is reported, so the answer is computed once, then.
func (r *checkerPureReports) roots() map[ssa.Value]bool {
	if r.marked != nil {
		return r.marked
	}
	r.marked = make(map[ssa.Value]bool)
	for _, v := range r.pure {
		switch {
		case v.PureReceiverRoot != nil:
			r.marked[v.PureReceiverRoot] = true
		case !r.other[v.Pos]:
			r.marked[v.Root] = true
		}
	}
	return r.marked
}
