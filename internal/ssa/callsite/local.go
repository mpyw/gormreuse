package callsite

import "golang.org/x/tools/go/ssa"

// LocalStores returns, in no particular order, every value stored into the
// local alloc. ok is false when a store may be missing: a closure that
// captures alloc stores into it, at any depth, or its address is used another
// way than to load and store it, in its function or in such a closure. A load
// that runs before any store reads the zero value.
func LocalStores(alloc *ssa.Alloc) (vals []ssa.Value, ok bool) {
	var visit func(addr ssa.Value) bool
	visit = func(addr ssa.Value) bool {
		for _, ref := range *addr.Referrers() {
			switch r := ref.(type) {
			case *ssa.Store:
				// A store in a closure may run at any point.
				if r.Addr != addr || addr != ssa.Value(alloc) {
					return false
				}
				vals = append(vals, r.Val)
			case *ssa.MakeClosure:
				closure := r.Fn.(*ssa.Function)
				for i, binding := range r.Bindings {
					if binding == addr && !visit(closure.FreeVars[i]) {
						return false
					}
				}
			case *ssa.UnOp, *ssa.DebugRef:
				// A load: an address has no other unary operation.
			default:
				return false
			}
		}
		return true
	}
	if !visit(alloc) {
		return nil, false
	}
	return vals, true
}
