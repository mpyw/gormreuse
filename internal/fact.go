package internal

import (
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/facts"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// factSources is what exportFacts reads: the directive sets and what
// validating each contract found.
//
//declscope:shared // run.go gathers what the facts are made of
type factSources struct {
	pureFuncs, immutableReturnFuncs *directive.DirectiveFuncSet
	immutableInputSet               *directive.ImmutableInputSet
	pureVerdicts                    *tracer.PureVerdicts
	distrustedReturn, failedInput   map[*ssa.Function]bool
	needsImmutableParam             map[*ssa.Function]bool
	tracer                          *tracer.RootTracer
}

// exportFacts exports a facts.Func for every exported function and method of
// the package that carries a directive, so that the packages that
// call it see what this pass found (#195):
//
//   - pure, whether the body broke the contract, and which parameters the
//     result may be;
//   - immutable-return, declared or inferred, only when the body keeps the
//     contract without relying on a function whose body breaks it;
//   - immutable-param, only when the function branches a parameter, which is
//     when its callers must pass an isolated value;
//   - immutable-input(name), only when the body keeps the contract.
//
// A contract that the body breaks is reported here, at the declaration, and
// still trusted in this package so that the report is not repeated at every
// call. Another package does not trust it: its author may never see this
// package's reports.
//
// A function in a skipped file, such as a generated one, exports nothing: its
// contracts are not validated, so another package must not trust them (#259).
//
//declscope:shared // run.go exports the facts
func exportFacts(pass *analysis.Pass, ssaInfo *buildssa.SSA, src factSources, skip func(*ssa.Function) bool, guard *panicGuard) {
	for _, fn := range ssaInfo.SrcFuncs {
		if obj, ok := fn.Object().(*types.Func); !ok || !obj.Exported() {
			continue // another package cannot name it
		}
		if skip(fn) {
			continue
		}
		guard.run(fn, func() {
			var f facts.Func
			if src.pureFuncs.Contains(fn) {
				f.Pure = true
				f.PureFailed = src.pureVerdicts.Failed[fn]
				f.Returned = src.pureVerdicts.Returned[fn]
				f.ReturnsOnlyParams = len(f.Returned) > 0 && src.tracer.PureReturnsOnlyParams(fn)
			}
			f.ImmutableReturn = src.immutableReturnFuncs.Contains(fn) && !src.distrustedReturn[fn]
			f.BranchesImmutableParam = src.needsImmutableParam[fn]
			if !src.failedInput[fn] {
				f.ImmutableInput = src.immutableInputSet.Callbacks(fn)
			}
			facts.Export(pass, fn, f)
		})
	}
}
