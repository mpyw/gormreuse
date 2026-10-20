package internal

import (
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
	fixGen              *fix.Generator           // Cached fix generator for all violations
	cfgs                *cfg.Analyzer            // CFG cache of the pass, shared with fixGen (#299)
}

// newChecker creates a new checker for a specific file.
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
		fixGen:              fixGen,
		cfgs:                cfgs,
	}
}

// checkFunction runs SSA analysis on a single function and reports violations.
//
//declscope:shared // run.go checks every function with it
func (c *checker) checkFunction(fn *ssa.Function) {
	analyzer := ssautil.NewAnalyzer(fn, c.rootTracer, c.needsImmutableParam, c.cfgs)
	violations := analyzer.Analyze()

	// Every diagnostic of a root carries the root's fix, not only the first:
	// the first may be suppressed by an ignore directive or already reported,
	// and an editor offers a quick fix only on the diagnostic that carries it
	// (#246). Each fix is complete on its own (#200), so the copies are
	// identical, and the drivers apply an edit that several fixes share once.
	// A root's fix is generated once, for the first diagnostic of the root that
	// is reported. It depends only on the root and its uses.
	fixes := make(map[ssa.Value][]analysis.SuggestedFix)
	fixOf := func(v pollution.Violation) []analysis.SuggestedFix {
		f, ok := fixes[v.Root]
		if !ok {
			f = c.fixGen.Generate(v)
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
			if !slices.ContainsFunc(fixFor, func(f pollution.Violation) bool { return f.Root == v.Root }) {
				fixFor = append(fixFor, v)
			}
		}
		if len(reuse) > 0 {
			c.reportViolation(reuse[0], func() []analysis.SuggestedFix {
				var fixes []analysis.SuggestedFix
				for _, fv := range fixFor {
					fixes = append(fixes, fixOf(fv)...)
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

	// Report with diagnostic
	c.pass.Report(analysis.Diagnostic{
		Pos:            pos,
		Message:        v.Message,
		SuggestedFixes: suggestedFixes,
	})
}
