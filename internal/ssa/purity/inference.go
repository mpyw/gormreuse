package purity

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// Inferencer
// =============================================================================

// Inferencer infers purity states of SSA values within a function.
type Inferencer struct {
	fn        *ssa.Function
	pureFuncs *directive.PureFuncSet
	cache     map[ssa.Value]State
	visiting  map[ssa.Value]bool
}

// NewInferencer creates a new purity inferencer for the given function.
func NewInferencer(fn *ssa.Function, pureFuncs *directive.PureFuncSet) *Inferencer {
	return &Inferencer{
		fn:        fn,
		pureFuncs: pureFuncs,
		cache:     make(map[ssa.Value]State),
		visiting:  make(map[ssa.Value]bool),
	}
}

// =============================================================================
// Value Analysis
// =============================================================================

// InferValue returns the purity state of the given SSA value.
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
//	InferValue(t1)                    // Start: Phi node
//	  → InferValue(t0)                // Edge 1: Call node
//	      → inferCall(t0)             // Session() → Clean
//	  → InferValue(db)                // Edge 2: Parameter
//	      → Depends(db)                 // Parameter → Depends
//	  → Merge: Clean ⊔ Depends(db)      // Result: Depends(db)
//
// Optimizations:
//   - cache: Memoizes results to avoid redundant traversal
//   - visiting: Detects cycles (returns Polluted for safety)
func (inf *Inferencer) InferValue(v ssa.Value) State {
	if v == nil {
		return Clean()
	}

	if state, ok := inf.cache[v]; ok {
		return state
	}

	if inf.visiting[v] {
		return Polluted() // Cycle detection
	}
	inf.visiting[v] = true
	defer delete(inf.visiting, v)

	state := inf.inferValueImpl(v)
	inf.cache[v] = state
	return state
}

func (inf *Inferencer) inferValueImpl(v ssa.Value) State {
	switch val := v.(type) {
	case *ssa.Parameter:
		// func foo(db *gorm.DB) { ... }
		//          ^^
		// Parameter's purity depends on what the caller passes.
		if typeutil.IsGormDB(val.Type()) {
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
		return inf.inferCall(val)

	case *ssa.Phi:
		// if cond {
		//     x = db.Session(...)  // Clean
		// } else {
		//     x = db               // Depends(db)
		// }
		// return x  // Phi node merges both branches → Depends(db)
		return inf.inferPhi(val)

	case *ssa.Extract:
		// tx, err := db.Begin()
		// ^^
		// Extract gets a value from a tuple (multiple return values).
		return inf.InferValue(val.Tuple)

	case *ssa.UnOp:
		// *ptr  // dereference
		// Trace through to the underlying value.
		return inf.InferValue(val.X)

	case *ssa.MakeClosure:
		// NOTE: This branch is defensive code - unlikely to be reached.
		// MakeClosure produces function types, not *gorm.DB.
		// We only call InferValue on *gorm.DB type values.
		// Kept for completeness in case of unexpected SSA patterns.
		for _, binding := range val.Bindings {
			if typeutil.IsGormDB(binding.Type()) {
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
		return inf.InferValue(val.X)

	case *ssa.Convert:
		// NOTE: Defensive code - rarely reached in practice.
		// Convert is for explicit type conversions (e.g., unsafe.Pointer).
		// Trace through - same underlying value.
		return inf.InferValue(val.X)

	case *ssa.TypeAssert:
		// v.(*gorm.DB)  // type assertion
		// ^^^^^^^^^^^^
		// Trace through - extracts the underlying value.
		return inf.InferValue(val.X)

	case *ssa.MakeInterface:
		// NOTE: Defensive code - unlikely to be reached.
		// MakeInterface produces interface{} type, not *gorm.DB.
		// Wrapping in interface - trace through.
		return inf.InferValue(val.X)

	case *ssa.Slice:
		// NOTE: Defensive code - unlikely to be reached.
		// Slice produces slice types, not *gorm.DB.
		// Trace through to the underlying slice.
		return inf.InferValue(val.X)

	default:
		// NOTE: Defensive code - should not be reached.
		// All known SSA types that can produce *gorm.DB are handled above.
		// Conservative: assume Polluted for unexpected cases.
		return Polluted()
	}
}

// =============================================================================
// Call Analysis
// =============================================================================

// inferCall analyzes a function/method call and returns its purity state.
//
// Examples:
//
//	db.Where("x")           → Polluted (non-pure method on *gorm.DB)
//	db.Session(&Session{})  → Clean    (pure builtin method)
//	pureHelper(db)          → Depends  (user-defined pure function)
//	nonPureHelper(db)       → Polluted (non-pure function with *gorm.DB arg)
//	fmt.Println("hello")    → Clean    (no *gorm.DB involved)
func (inf *Inferencer) inferCall(call *ssa.Call) State {
	// Interface method calls (call.Call.Method != nil) are not supported
	// because IsGormDB only matches *gorm.DB (concrete pointer type).
	// GORM's DB is a struct, so all method calls go through StaticCallee.

	// Static call (concrete type or function)
	callee := call.Call.StaticCallee()
	if callee == nil {
		// Dynamic call (function pointer, etc.) - can't analyze
		return Polluted()
	}

	// Method call on *gorm.DB (concrete type)
	// e.g., (*gorm.DB).Where(db, "x")
	if sig := callee.Signature; sig != nil && sig.Recv() != nil && typeutil.IsGormDB(sig.Recv().Type()) {
		if typeutil.IsPureFunctionBuiltin(callee.Name()) {
			return Clean()
		}
		return Polluted()
	}

	// User-defined pure function: //gormreuse:pure func helper(db *gorm.DB) *gorm.DB
	if inf.pureFuncs.Contains(callee) {
		return inf.inferPureUserFuncCall(call)
	}

	// Non-pure function receiving *gorm.DB - assume it pollutes
	for _, arg := range call.Call.Args {
		if typeutil.IsGormDB(arg.Type()) {
			return Polluted()
		}
	}

	return Clean()
}

// inferPureUserFuncCall analyzes a call to a user-defined pure function.
//
// The result depends on what *gorm.DB arguments are passed:
//
//	//gormreuse:pure
//	func identity(db *gorm.DB) *gorm.DB { return db }
//
//	identity(param)           → Depends(param)  (traces to parameter)
//	identity(db.Session(...)) → Clean           (arg is Clean)
//	identity(pollutedDB)      → Polluted        (arg is Polluted)
func (inf *Inferencer) inferPureUserFuncCall(call *ssa.Call) State {
	var deps []*ssa.Parameter
	for _, arg := range call.Call.Args {
		if !typeutil.IsGormDB(arg.Type()) {
			continue
		}

		if param, ok := inf.traceToParameter(arg); ok {
			deps = append(deps, param)
		} else {
			argState := inf.InferValue(arg)
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

// inferPhi analyzes a Phi node, which merges values from different control flow paths.
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
func (inf *Inferencer) inferPhi(phi *ssa.Phi) State {
	if len(phi.Edges) == 0 {
		return Clean()
	}

	result := inf.InferValue(phi.Edges[0])
	for _, edge := range phi.Edges[1:] {
		result = result.Merge(inf.InferValue(edge))
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
func (inf *Inferencer) traceToParameter(v ssa.Value) (*ssa.Parameter, bool) {
	return inf.traceToParameterImpl(v, make(map[ssa.Value]bool))
}

func (inf *Inferencer) traceToParameterImpl(v ssa.Value, visited map[ssa.Value]bool) (*ssa.Parameter, bool) {
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
			p, ok := inf.traceToParameterImpl(edge, visited)
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
		return inf.traceToParameterImpl(val.X, visited)

	case *ssa.ChangeType:
		// MyDB(db)  // type alias conversion
		// Same underlying value, trace through.
		return inf.traceToParameterImpl(val.X, visited)

	case *ssa.Convert:
		// (*gorm.DB)(ptr)  // type conversion
		// Same underlying value, trace through.
		return inf.traceToParameterImpl(val.X, visited)

	case *ssa.MakeInterface:
		// var i interface{} = db
		// Wrapping in interface, trace through.
		return inf.traceToParameterImpl(val.X, visited)

	default:
		// db.Where("x")  // Call - can't trace through
		// h.db           // FieldAddr - can't trace through
		// Anything that transforms the value breaks the trace.
		return nil, false
	}
}
