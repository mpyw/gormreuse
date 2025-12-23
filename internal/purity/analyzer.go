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
//
// This function recursively traverses the SSA value graph to determine purity.
// Each SSA value may reference other values (e.g., Phi edges, Call arguments,
// UnOp operands), and we follow these references to trace back to the origin.
//
// Traversal example:
//
//	func f(db *gorm.DB, cond bool) *gorm.DB {
//	    var x *gorm.DB
//	    if cond {
//	        x = db.Session(&Session{})
//	    } else {
//	        x = db
//	    }
//	    return x
//	}
//
// SSA form (simplified):
//
//	entry:
//	    if cond goto then else else
//	then:
//	    t0 = db.Session(...)    ← Call node
//	    goto merge
//	else:
//	    goto merge
//	merge:
//	    t1 = phi [then: t0, else: db]  ← Phi node
//	    return t1
//
// Recursive traversal for t1 (Phi):
//
//	AnalyzeValue(t1)                    // Start: Phi node
//	  → AnalyzeValue(t0)                // Edge 1: Call node
//	      → analyzeCall(t0)             // Session() → Clean
//	  → AnalyzeValue(db)                // Edge 2: Parameter
//	      → Depends(db)                 // Parameter → Depends
//	  → Merge: Clean ⊔ Depends(db)      // Result: Depends(db)
//
// Optimizations:
//   - cache: Memoizes results to avoid redundant traversal
//   - visiting: Detects cycles (returns Polluted for safety)
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

// analyzeCall analyzes a function/method call and returns its purity state.
//
// Examples:
//
//	db.Where("x")           → Polluted (non-pure method on *gorm.DB)
//	db.Session(&Session{})  → Clean    (pure builtin method)
//	pureHelper(db)          → Depends  (user-defined pure function)
//	nonPureHelper(db)       → Polluted (non-pure function with *gorm.DB arg)
//	fmt.Println("hello")    → Clean    (no *gorm.DB involved)
func (a *Analyzer) analyzeCall(call *ssa.Call) State {
	// Interface method call: var db gorm.DB; db.Where(...)
	if call.Call.Method != nil {
		return a.analyzeInterfaceMethodCall(call)
	}

	// Static call (concrete type or function)
	callee := call.Call.StaticCallee()
	if callee == nil {
		// Dynamic call (function pointer, etc.) - can't analyze
		return Polluted()
	}

	// Method call on *gorm.DB (concrete type)
	// e.g., (*gorm.DB).Where(db, "x")
	if sig := callee.Signature; sig != nil && sig.Recv() != nil && a.checker.IsGormDB(sig.Recv().Type()) {
		if a.checker.IsPureBuiltinMethod(callee.Name()) {
			return Clean()
		}
		return Polluted()
	}

	// User-defined pure function: //gormreuse:pure func helper(db *gorm.DB) *gorm.DB
	if a.checker.IsPureUserFunc(callee) {
		return a.analyzePureUserFuncCall(call)
	}

	// Non-pure function receiving *gorm.DB - assume it pollutes
	for _, arg := range call.Call.Args {
		if a.checker.IsGormDB(arg.Type()) {
			return Polluted()
		}
	}

	return Clean()
}

// analyzeInterfaceMethodCall analyzes a method call through an interface.
//
// Examples:
//
//	var db gorm.DB  // interface type
//	db.Session(&Session{})  → Clean    (pure builtin)
//	db.Where("x")           → Polluted (non-pure method)
//	db.Find(&users)         → Polluted (non-pure method)
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

// analyzePureUserFuncCall analyzes a call to a user-defined pure function.
//
// The result depends on what *gorm.DB arguments are passed:
//
//	//gormreuse:pure
//	func identity(db *gorm.DB) *gorm.DB { return db }
//
//	identity(param)           → Depends(param)  (traces to parameter)
//	identity(db.Session(...)) → Clean           (arg is Clean)
//	identity(pollutedDB)      → Polluted        (arg is Polluted)
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

// analyzePhi analyzes a Phi node, which merges values from different control flow paths.
//
// SSA uses Phi nodes at join points where multiple branches converge:
//
//	func f(db *gorm.DB, cond bool) *gorm.DB {
//	    var x *gorm.DB
//	    if cond {
//	        x = db.Session(&Session{})  // Block 1: Clean
//	    } else {
//	        x = db                       // Block 2: Depends(db)
//	    }
//	    return x  // Phi node: merge Block1 and Block2
//	}
//
// SSA representation:
//
//	Block 3:
//	    t2 = phi [Block1: t0, Block2: db]  ← Phi merges both values
//	    return t2
//
// Merge result: Clean ⊔ Depends(db) = Depends(db)
//
// All incoming edges are analyzed and merged using lattice rules:
//   - Clean ⊔ Clean = Clean
//   - Clean ⊔ Depends(p) = Depends(p)
//   - * ⊔ Polluted = Polluted (short-circuits)
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

// traceToParameter traces a value back to a function parameter if possible.
//
// This is used to determine if a value is directly derived from a parameter
// without any transformations that would change its purity.
//
// Examples that trace successfully:
//
//	func f(db *gorm.DB) {
//	    x := db           // x traces to db parameter
//	    y := (*gorm.DB)(x) // y traces to db parameter (type conversion)
//	}
//
// Examples that fail to trace:
//
//	func f(db *gorm.DB) {
//	    x := db.Where("x")  // x does NOT trace (method call)
//	    y := getDB()        // y does NOT trace (function call)
//	}
//
// For Phi nodes, all edges must trace to the SAME parameter:
//
//	func f(db *gorm.DB, cond bool) {
//	    var x *gorm.DB
//	    if cond { x = db } else { x = db }  // OK: both edges → db
//	    // x traces to db
//	}
//
//	func f(db1, db2 *gorm.DB, cond bool) {
//	    var x *gorm.DB
//	    if cond { x = db1 } else { x = db2 }  // FAIL: different params
//	    // x does NOT trace
//	}
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
		// func f(db *gorm.DB) { use(db) }
		//                           ^^
		// Found the parameter - trace successful.
		return val, true

	case *ssa.Phi:
		// if cond { x = db } else { x = db }
		//           ^^^^^^          ^^^^^^
		// All edges must trace to the SAME parameter.
		// If edges trace to different parameters, trace fails.
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
		// *ptr  // dereference
		// Trace through to the underlying value.
		return a.traceToParameterImpl(val.X, visited)

	case *ssa.ChangeType:
		// MyDB(db)  // type alias conversion
		// Same underlying value, trace through.
		return a.traceToParameterImpl(val.X, visited)

	case *ssa.Convert:
		// (*gorm.DB)(ptr)  // type conversion
		// Same underlying value, trace through.
		return a.traceToParameterImpl(val.X, visited)

	case *ssa.MakeInterface:
		// var i interface{} = db
		// Wrapping in interface, trace through.
		return a.traceToParameterImpl(val.X, visited)

	default:
		// db.Where("x")  // Call - can't trace through
		// h.db           // FieldAddr - can't trace through
		// Anything that transforms the value breaks the trace.
		return nil, false
	}
}

// =============================================================================
// Return Analysis
// =============================================================================

// AnalyzeReturn returns the merged purity state of all *gorm.DB return values.
//
// This function traverses all basic blocks to find return statements and analyzes
// each *gorm.DB return value. Multiple return values are merged using lattice rules.
//
// Examples:
//
//	func f(db *gorm.DB) *gorm.DB {
//	    return db.Session(&Session{})  // Single return → Clean
//	}
//
//	func f(db *gorm.DB, cond bool) *gorm.DB {
//	    if cond {
//	        return db.Session(&Session{})  // Return 1: Clean
//	    }
//	    return db                           // Return 2: Depends(db)
//	}
//	// Result: Clean ⊔ Depends(db) = Depends(db) ← valid for pure function
//
//	func f(db *gorm.DB, cond bool) *gorm.DB {
//	    if cond {
//	        return db.Session(&Session{})  // Return 1: Clean
//	    }
//	    return db.Where("x")                // Return 2: Polluted
//	}
//	// Result: Clean ⊔ Polluted = Polluted ← INVALID for pure function
//
// Short-circuit: Returns immediately when Polluted state is encountered.
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
