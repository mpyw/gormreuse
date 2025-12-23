// Package purity provides 3-state purity analysis for *gorm.DB values.
package purity

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// PurityChecker provides methods to check purity of functions and types.
// This interface is used to avoid circular dependencies with the internal package.
type PurityChecker interface {
	// IsGormDB returns true if the type is *gorm.DB.
	IsGormDB(t types.Type) bool
	// IsPureBuiltinMethod returns true if the method name is a pure builtin method.
	IsPureBuiltinMethod(methodName string) bool
	// IsPureUserFunc returns true if the function is marked as pure by user.
	IsPureUserFunc(fn *ssa.Function) bool
}

// Analyzer analyzes purity states of SSA values within a function.
type Analyzer struct {
	fn      *ssa.Function
	checker PurityChecker
	cache   map[ssa.Value]State
	// visiting tracks values being analyzed to detect cycles
	visiting map[ssa.Value]bool
}

// NewAnalyzer creates a new purity analyzer for the given function.
func NewAnalyzer(fn *ssa.Function, checker PurityChecker) *Analyzer {
	return &Analyzer{
		fn:       fn,
		checker:  checker,
		cache:    make(map[ssa.Value]State),
		visiting: make(map[ssa.Value]bool),
	}
}

// AnalyzeValue returns the purity state of the given SSA value.
// It handles cycles by returning Polluted for values being visited.
func (a *Analyzer) AnalyzeValue(v ssa.Value) State {
	if v == nil {
		return Clean() // nil is always clean
	}

	// Check cache first
	if state, ok := a.cache[v]; ok {
		return state
	}

	// Detect cycles
	if a.visiting[v] {
		// Conservative: cycles are treated as Polluted
		return Polluted()
	}
	a.visiting[v] = true
	defer func() { delete(a.visiting, v) }()

	state := a.analyzeValueImpl(v)
	a.cache[v] = state
	return state
}

// analyzeValueImpl implements the actual analysis logic for each SSA value type.
func (a *Analyzer) analyzeValueImpl(v ssa.Value) State {
	switch val := v.(type) {
	case *ssa.Parameter:
		// Parameter's purity depends on itself
		if a.checker.IsGormDB(val.Type()) {
			return Depends(val)
		}
		return Clean()

	case *ssa.Const:
		// Constants (including nil) are always clean
		return Clean()

	case *ssa.Call:
		return a.analyzeCall(val)

	case *ssa.Phi:
		return a.analyzePhi(val)

	case *ssa.Extract:
		// Extract from tuple - trace back to the tuple source
		return a.AnalyzeValue(val.Tuple)

	case *ssa.UnOp:
		// Dereference or other unary op - trace through
		return a.AnalyzeValue(val.X)

	case *ssa.MakeClosure:
		// Closure - conservative: treat as Polluted if it captures *gorm.DB
		for _, binding := range val.Bindings {
			if a.checker.IsGormDB(binding.Type()) {
				return Polluted()
			}
		}
		return Clean()

	case *ssa.FieldAddr, *ssa.Field:
		// Field access - conservative: assume Polluted
		return Polluted()

	case *ssa.IndexAddr, *ssa.Index:
		// Index access - conservative: assume Polluted
		return Polluted()

	case *ssa.Lookup:
		// Map lookup - conservative: assume Polluted
		return Polluted()

	case *ssa.ChangeType, *ssa.Convert:
		// Type conversion - trace through
		if ct, ok := val.(*ssa.ChangeType); ok {
			return a.AnalyzeValue(ct.X)
		}
		if cv, ok := val.(*ssa.Convert); ok {
			return a.AnalyzeValue(cv.X)
		}
		return Polluted()

	case *ssa.TypeAssert:
		// Type assertion - trace through but could fail at runtime
		return a.AnalyzeValue(val.X)

	case *ssa.MakeInterface:
		// Interface creation - trace through
		return a.AnalyzeValue(val.X)

	case *ssa.Slice:
		// Slice operation - trace through
		return a.AnalyzeValue(val.X)

	default:
		// Unknown - conservative: assume Polluted
		return Polluted()
	}
}

// analyzeCall analyzes a call expression.
func (a *Analyzer) analyzeCall(call *ssa.Call) State {
	// Interface method call
	if call.Call.Method != nil {
		recv := call.Call.Value
		if !a.checker.IsGormDB(recv.Type()) {
			return Clean()
		}

		methodName := call.Call.Method.Name()
		if a.checker.IsPureBuiltinMethod(methodName) {
			// Pure method returns Clean regardless of receiver
			return Clean()
		}
		// Non-pure method - result is Polluted
		return Polluted()
	}

	// Static call
	callee := call.Call.StaticCallee()
	if callee == nil {
		// Dynamic call - conservative: Polluted
		return Polluted()
	}

	// Check if it's a method call on *gorm.DB
	sig := callee.Signature
	if sig != nil && sig.Recv() != nil && a.checker.IsGormDB(sig.Recv().Type()) {
		methodName := callee.Name()
		if a.checker.IsPureBuiltinMethod(methodName) {
			return Clean()
		}
		return Polluted()
	}

	// Check if it's a user-defined pure function
	if a.checker.IsPureUserFunc(callee) {
		// Pure function - analyze what it returns
		// For now, assume pure functions return Depends on their *gorm.DB args
		// This is conservative but safe
		var deps []*ssa.Parameter
		for _, arg := range call.Call.Args {
			if a.checker.IsGormDB(arg.Type()) {
				// If arg is a parameter, add it to deps
				if param, ok := a.traceToParameter(arg); ok {
					deps = append(deps, param)
				} else {
					// Arg is not directly traceable to a parameter
					// Merge with the arg's state
					argState := a.AnalyzeValue(arg)
					if argState.IsPolluted() {
						return Polluted()
					}
					if argState.IsDepends() {
						deps = append(deps, argState.Deps()...)
					}
				}
			}
		}
		if len(deps) > 0 {
			return Depends(deps...)
		}
		return Clean()
	}

	// Non-pure function call with *gorm.DB argument - Polluted
	for _, arg := range call.Call.Args {
		if a.checker.IsGormDB(arg.Type()) {
			return Polluted()
		}
	}

	return Clean()
}

// analyzePhi analyzes a phi node by merging all incoming edges.
func (a *Analyzer) analyzePhi(phi *ssa.Phi) State {
	if len(phi.Edges) == 0 {
		return Clean()
	}

	result := a.AnalyzeValue(phi.Edges[0])
	for _, edge := range phi.Edges[1:] {
		edgeState := a.AnalyzeValue(edge)
		result = result.Merge(edgeState)
		// Early exit if already Polluted
		if result.IsPolluted() {
			return result
		}
	}
	return result
}

// traceToParameter traces a value back to a function parameter if possible.
func (a *Analyzer) traceToParameter(v ssa.Value) (*ssa.Parameter, bool) {
	visited := make(map[ssa.Value]bool)
	return a.traceToParameterImpl(v, visited)
}

func (a *Analyzer) traceToParameterImpl(v ssa.Value, visited map[ssa.Value]bool) (*ssa.Parameter, bool) {
	if visited[v] {
		return nil, false
	}
	visited[v] = true

	switch val := v.(type) {
	case *ssa.Parameter:
		return val, true
	case *ssa.Phi:
		// For phi, check if all edges trace to the same parameter
		var param *ssa.Parameter
		for _, edge := range val.Edges {
			p, ok := a.traceToParameterImpl(edge, visited)
			if !ok {
				return nil, false
			}
			if param == nil {
				param = p
			} else if param != p {
				return nil, false // Different parameters
			}
		}
		return param, param != nil
	case *ssa.UnOp:
		return a.traceToParameterImpl(val.X, visited)
	case *ssa.ChangeType:
		return a.traceToParameterImpl(val.X, visited)
	case *ssa.Convert:
		return a.traceToParameterImpl(val.X, visited)
	case *ssa.MakeInterface:
		return a.traceToParameterImpl(val.X, visited)
	default:
		return nil, false
	}
}

// AnalyzeReturn returns the merged purity state of all return values
// that are *gorm.DB type.
func (a *Analyzer) AnalyzeReturn() State {
	result := Clean()

	for _, block := range a.fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}

			for _, res := range ret.Results {
				if res == nil {
					continue
				}
				if !a.checker.IsGormDB(res.Type()) {
					continue
				}

				state := a.AnalyzeValue(res)
				result = result.Merge(state)

				// Early exit if already Polluted
				if result.IsPolluted() {
					return result
				}
			}
		}
	}

	return result
}
