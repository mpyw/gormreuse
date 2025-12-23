package purity

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Purity Checker Interface
// =============================================================================

// PurityChecker provides methods to check purity of functions and types.
type PurityChecker interface {
	IsGormDB(t types.Type) bool
	IsPureBuiltinMethod(methodName string) bool
	IsPureUserFunc(fn *ssa.Function) bool
}

// =============================================================================
// Analyzer
// =============================================================================

// Analyzer analyzes purity states of SSA values within a function.
type Analyzer struct {
	fn       *ssa.Function
	checker  PurityChecker
	cache    map[ssa.Value]State
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

// =============================================================================
// Value Analysis
// =============================================================================

// AnalyzeValue returns the purity state of the given SSA value.
func (a *Analyzer) AnalyzeValue(v ssa.Value) State {
	if v == nil {
		return Clean()
	}

	if state, ok := a.cache[v]; ok {
		return state
	}

	if a.visiting[v] {
		return Polluted() // Cycle detection
	}
	a.visiting[v] = true
	defer delete(a.visiting, v)

	state := a.analyzeValueImpl(v)
	a.cache[v] = state
	return state
}

func (a *Analyzer) analyzeValueImpl(v ssa.Value) State {
	switch val := v.(type) {
	case *ssa.Parameter:
		// func foo(db *gorm.DB) { ... }
		//          ^^
		// Parameter's purity depends on what the caller passes.
		if a.checker.IsGormDB(val.Type()) {
			return Depends(val)
		}
		return Clean()

	case *ssa.Const:
		// return nil
		//        ^^^
		// Constants (including nil) are always clean.
		return Clean()

	case *ssa.Call:
		// db.Where("x")           // method call
		// pureHelper(db)          // function call
		// db.Session(&Session{})  // pure method call
		return a.analyzeCall(val)

	case *ssa.Phi:
		// if cond {
		//     x = db.Session(...)  // Clean
		// } else {
		//     x = db               // Depends(db)
		// }
		// return x  // Phi node merges both branches → Depends(db)
		return a.analyzePhi(val)

	case *ssa.Extract:
		// tx, err := db.Begin()
		// ^^
		// Extract gets a value from a tuple (multiple return values).
		return a.AnalyzeValue(val.Tuple)

	case *ssa.UnOp:
		// *ptr  // dereference
		// Trace through to the underlying value.
		return a.AnalyzeValue(val.X)

	case *ssa.MakeClosure:
		// f := func() { db.Find(nil) }
		//      ^^^^^^^^^^^^^^^^^^^^^^^^^
		// Closure capturing *gorm.DB is treated as Polluted
		// because we can't track what happens inside.
		for _, binding := range val.Bindings {
			if a.checker.IsGormDB(binding.Type()) {
				return Polluted()
			}
		}
		return Clean()

	case *ssa.FieldAddr, *ssa.Field:
		// h.db  // struct field access
		// ^^^^
		// Conservative: can't track field origin, assume Polluted.
		return Polluted()

	case *ssa.IndexAddr, *ssa.Index:
		// dbs[0]  // slice/array index
		// ^^^^^^
		// Conservative: can't track which element, assume Polluted.
		return Polluted()

	case *ssa.Lookup:
		// m["key"]  // map lookup
		// ^^^^^^^^
		// Conservative: can't track map contents, assume Polluted.
		return Polluted()

	case *ssa.ChangeType:
		// MyDB(db)  // type alias conversion
		// ^^^^^^^^
		// Trace through - same underlying value.
		return a.AnalyzeValue(val.X)

	case *ssa.Convert:
		// (*gorm.DB)(ptr)  // type conversion
		// Trace through - same underlying value.
		return a.AnalyzeValue(val.X)

	case *ssa.TypeAssert:
		// v.(*gorm.DB)  // type assertion
		// ^^^^^^^^^^^^
		// Trace through - extracts the underlying value.
		return a.AnalyzeValue(val.X)

	case *ssa.MakeInterface:
		// var i interface{} = db
		//                     ^^
		// Wrapping in interface - trace through.
		return a.AnalyzeValue(val.X)

	case *ssa.Slice:
		// dbs[1:3]  // slice operation
		// ^^^^^^^^
		// Trace through to the underlying slice.
		return a.AnalyzeValue(val.X)

	default:
		// Unknown SSA value type - conservative: assume Polluted.
		return Polluted()
	}
}

// =============================================================================
// Call Analysis
// =============================================================================

func (a *Analyzer) analyzeCall(call *ssa.Call) State {
	// Interface method call
	if call.Call.Method != nil {
		return a.analyzeInterfaceMethodCall(call)
	}

	// Static call
	callee := call.Call.StaticCallee()
	if callee == nil {
		return Polluted()
	}

	// Method call on *gorm.DB
	if sig := callee.Signature; sig != nil && sig.Recv() != nil && a.checker.IsGormDB(sig.Recv().Type()) {
		if a.checker.IsPureBuiltinMethod(callee.Name()) {
			return Clean()
		}
		return Polluted()
	}

	// User-defined pure function
	if a.checker.IsPureUserFunc(callee) {
		return a.analyzePureUserFuncCall(call)
	}

	// Non-pure function with *gorm.DB argument
	for _, arg := range call.Call.Args {
		if a.checker.IsGormDB(arg.Type()) {
			return Polluted()
		}
	}

	return Clean()
}

func (a *Analyzer) analyzeInterfaceMethodCall(call *ssa.Call) State {
	recv := call.Call.Value
	if !a.checker.IsGormDB(recv.Type()) {
		return Clean()
	}

	if a.checker.IsPureBuiltinMethod(call.Call.Method.Name()) {
		return Clean()
	}
	return Polluted()
}

func (a *Analyzer) analyzePureUserFuncCall(call *ssa.Call) State {
	var deps []*ssa.Parameter
	for _, arg := range call.Call.Args {
		if !a.checker.IsGormDB(arg.Type()) {
			continue
		}

		if param, ok := a.traceToParameter(arg); ok {
			deps = append(deps, param)
		} else {
			argState := a.AnalyzeValue(arg)
			if argState.IsPolluted() {
				return Polluted()
			}
			if argState.IsDepends() {
				deps = append(deps, argState.Deps()...)
			}
		}
	}

	if len(deps) > 0 {
		return Depends(deps...)
	}
	return Clean()
}

// =============================================================================
// Phi Analysis
// =============================================================================

func (a *Analyzer) analyzePhi(phi *ssa.Phi) State {
	if len(phi.Edges) == 0 {
		return Clean()
	}

	result := a.AnalyzeValue(phi.Edges[0])
	for _, edge := range phi.Edges[1:] {
		result = result.Merge(a.AnalyzeValue(edge))
		if result.IsPolluted() {
			return result
		}
	}
	return result
}

// =============================================================================
// Parameter Tracing
// =============================================================================

func (a *Analyzer) traceToParameter(v ssa.Value) (*ssa.Parameter, bool) {
	return a.traceToParameterImpl(v, make(map[ssa.Value]bool))
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
		var param *ssa.Parameter
		for _, edge := range val.Edges {
			p, ok := a.traceToParameterImpl(edge, visited)
			if !ok {
				return nil, false
			}
			if param == nil {
				param = p
			} else if param != p {
				return nil, false
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

// =============================================================================
// Return Analysis
// =============================================================================

// AnalyzeReturn returns the merged purity state of all *gorm.DB return values.
func (a *Analyzer) AnalyzeReturn() State {
	result := Clean()

	for _, block := range a.fn.Blocks {
		for _, instr := range block.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}

			for _, res := range ret.Results {
				if res == nil || !a.checker.IsGormDB(res.Type()) {
					continue
				}

				result = result.Merge(a.AnalyzeValue(res))
				if result.IsPolluted() {
					return result
				}
			}
		}
	}

	return result
}
