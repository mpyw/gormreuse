package tracer

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/facts"
)

// PureVerdicts is what validating the //gormreuse:pure functions found. The
// purity package fills it before the reuse analysis runs.
type PureVerdicts struct {
	// Failed lists the pure functions whose bodies definitively break the
	// contract (pollute or leak an argument). They must NOT be trusted as pure
	// at call sites, since that would hide reuse of the value they leak (#66).
	Failed map[*ssa.Function]bool

	// Returned lists, for each pure function that keeps the contract, the
	// indexes into fn.Params of the parameters its result may be (#188). A
	// pure function does not pollute its argument, but `return db` hands the
	// same *gorm.DB back, so a branch from the result is a branch from the
	// argument.
	Returned map[*ssa.Function][]int

	// Imported holds the verdicts that other packages exported for their pure
	// functions (#195). Failed and Returned hold only this package's.
	Imported *facts.Table
}

// HasFailed reports whether fn is a pure function that broke its contract. The
// verdicts are keyed by the declared function, and fn may be a generic instance
// or a bound method wrapper of it (#180).
func (p *PureVerdicts) HasFailed(fn *ssa.Function) bool {
	if p == nil {
		return false
	}
	declared := directive.DeclaredFunc(fn)
	return p.Failed[declared] || p.Imported.Of(declared).PureFailed
}

// ReturnedArgs returns the arguments of a call to fn (args, as in
// CallCommon.Args) that its result may be. A bound method wrapper (f := x.M;
// f(a)) takes no receiver argument, so the indexes are shifted by the
// parameters it lacks; the receiver itself is never in Returned. The count of
// parameters comes from the signature, since a function of another package
// has no Params.
func (p *PureVerdicts) ReturnedArgs(fn *ssa.Function, args []ssa.Value) []ssa.Value {
	if p == nil {
		return nil
	}
	declared := directive.DeclaredFunc(fn)
	indexes := p.Returned[declared]
	if len(indexes) == 0 {
		indexes = p.Imported.Of(declared).Returned
	}
	if len(indexes) == 0 {
		return nil
	}
	nparams := declared.Signature.Params().Len()
	if declared.Signature.Recv() != nil {
		nparams++
	}
	shift := nparams - len(args)
	var returned []ssa.Value
	for _, i := range indexes {
		if j := i - shift; j >= 0 && j < len(args) {
			returned = append(returned, args[j])
		}
	}
	return returned
}
