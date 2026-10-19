package tracer

import (
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
)

// traceAllocStore traces a local variable (Alloc) by finding Store instructions.
//
// In SSA, local variables are represented as:
//
//	t1 = Alloc *gorm.DB (q)     // Allocate space for variable q
//	Store t1 <value>            // Store a value into q
//	t2 = UnOp * t1              // Load value from q (dereference)
//
// This function finds the Store instruction that writes to the Alloc.
//
//declscope:shared // root.go's traceNonCall and tracePointerLoad send local variables here
func (t *RootTracer) traceAllocStore(alloc *ssa.Alloc, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// Single-root: trace the first value stored into the Alloc.
	if vals := AllocStoredValues(alloc); len(vals) > 0 {
		return t.trace(vals[0], visited, loopInfo)
	}
	return nil
}

// AllocStoredValues returns, in program order, the values stored into alloc.
// Shared by traceAllocStore (first), traceAllAllocStores (all), the Scopes
// callback resolver, and scopeswarn.
func AllocStoredValues(alloc *ssa.Alloc) []ssa.Value {
	fn := alloc.Parent()
	if fn == nil {
		return nil
	}
	var vals []ssa.Value
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && store.Addr == alloc {
				vals = append(vals, store.Val)
			}
		}
	}
	return vals
}

// FieldStoredValues returns, in program order, the values stored into the same
// struct field as fa. Shared by traceFieldStore (first), traceAllFieldStores
// (all), and the Scopes callback resolver.
//
// With a load, only the stores that can reach it are returned (see
// fieldStores). With a nil load, every store in the function is returned.
func FieldStoredValues(fa *ssa.FieldAddr, load ssa.Instruction) []ssa.Value {
	root, path := fieldAddrPath(fa)
	return fieldStoredValuesAt(fa.Parent(), root, path, load)
}

// fieldValueStoredValues is FieldStoredValues for a field read from a struct
// held in a register rather than through its address (#181):
//
//	t4 = *t0             // e := embedded{q}; e.Where("a")
//	t5 = t4.DB           // Field, not FieldAddr: promoted methods read this way
//
// A Field chain over a load resolves to the address that was loaded, so it
// matches the same stores as a FieldAddr chain. A Field chain over a call that
// returns a struct yields that call's FieldRoot. A struct that a Phi merges
// yields the field of every edge.
//
//declscope:shared // root.go's traceNonCall and traceAll resolve a field read from a register with it
func fieldValueStoredValues(f *ssa.Field) []ssa.Value {
	return structFieldStoredValues(f, nil, make(map[ssa.Value]bool))
}

// structFieldStoredValues returns the values of the field that path selects
// from the struct value x. seen stops Phi cycles through loops.
func structFieldStoredValues(x ssa.Value, path []int, seen map[ssa.Value]bool) []ssa.Value {
	for {
		field, ok := x.(*ssa.Field)
		if !ok {
			break
		}
		path = append([]int{field.Field}, path...)
		x = field.X
	}
	switch base := x.(type) {
	case *ssa.Call:
		return []ssa.Value{&FieldRoot{Call: base, Path: path}}
	case *ssa.Phi:
		if seen[base] {
			return nil
		}
		seen[base] = true
		var vals []ssa.Value
		for _, edge := range base.Edges {
			vals = append(vals, structFieldStoredValues(edge, path, seen)...)
		}
		return vals
	case *ssa.UnOp:
		if base.Op != token.MUL {
			return nil
		}
		root, prefix := base.X, []int(nil)
		if fa, ok := base.X.(*ssa.FieldAddr); ok {
			root, prefix = fieldAddrPath(fa)
		}
		return fieldStoredValuesAt(base.Parent(), root, append(prefix, path...), base)
	default:
		return nil
	}
}

// fieldStoredValuesAt returns the values stored into the field that path
// selects from root, as read by load in fn. Each address root may stand for
// (see fieldBaseAddrs) is searched in the function that defines it, or in fn
// for one without a function, such as a global. Only the stores that reach load
// are returned when the address is defined in fn. Stores in an enclosing
// function have no CFG path to a load in a closure, so all of them are returned.
func fieldStoredValuesAt(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction) []ssa.Value {
	var vals []ssa.Value
	for _, base := range fieldBaseAddrs(root, make(map[ssa.Value]bool)) {
		baseFn, baseLoad := base.Parent(), load
		if baseFn == nil {
			baseFn = fn
		}
		if baseFn != fn {
			baseLoad = nil
		}
		vals = append(vals, fieldStoredValuesFrom(baseFn, base, path, baseLoad, make(map[storedFieldRef]bool))...)
	}
	return vals
}

// storedFieldRef identifies one (aggregate, field path, load) search. Every
// path visited during a single FieldStoredValues query is a suffix of the
// original path, so its length identifies it uniquely.
type storedFieldRef struct {
	root  ssa.Value
	depth int
	load  ssa.Instruction
}

// fieldStoredValuesFrom returns, in program order, the values stored into the
// field that path selects from root.
//
// Besides the direct stores it follows whole-aggregate copies into any prefix of
// path, which is required because the SSA builder does not initialize a
// composite literal in place at its destination: `h := T{f: q}` fills a separate
// `local T (complit)` temporary and copies the finished struct over.
//
//	t2 = local T (h)
//	t3 = local T (complit)
//	t4 = &t3.f
//	*t4 = q              // the store lives under the temporary
//	t5 = *t3             // load the finished literal
//	*t2 = t5             // copy it into h
//
// A FieldAddr on t2 has no matching field store of its own; following
// `*t2 = *t3` back to t3 recovers the store of q, as it was when t3 was
// loaded. seen guards against aggregates that copy into each other.
func fieldStoredValuesFrom(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction, seen map[storedFieldRef]bool) []ssa.Value {
	ref := storedFieldRef{root: root, depth: len(path), load: load}
	if fn == nil || root == nil || len(path) == 0 || seen[ref] {
		return nil
	}
	seen[ref] = true

	stores, open := reachingFieldStores(fn, root, path, load)
	var vals []ssa.Value
	if call, ok := root.(*ssa.Call); ok && open {
		// A field reached through a pointer that a call returned holds what
		// the call put there until a store replaces it (#250). That value is
		// the call's FieldRoot, as for a struct returned by value.
		vals = append(vals, &FieldRoot{Call: call, Path: slices.Clone(path)})
	}
	for _, store := range stores {
		storePath, _ := fieldStorePath(store, root)
		if len(storePath) == len(path) {
			vals = append(vals, store.Val)
			continue
		}
		// A whole sub-aggregate was copied over a prefix of the path being
		// read; resume the search at the address it was loaded from, for the
		// remainder of the path. A struct a call returned has no stores to
		// follow: its field is a FieldRoot (#181).
		switch agg := store.Val.(type) {
		case *ssa.UnOp:
			if agg.Op == token.MUL {
				vals = append(vals, fieldStoredValuesFrom(fn, agg.X, path[len(storePath):], agg, seen)...)
			}
		case *ssa.Call:
			vals = append(vals, &FieldRoot{Call: agg, Path: slices.Clone(path[len(storePath):])})
		}
	}
	return vals
}

// fieldStorePath returns the field path that store writes below root. ok is
// false when it writes somewhere else. A store to root itself has an empty
// path.
func fieldStorePath(store *ssa.Store, root ssa.Value) (path []int, ok bool) {
	storeRoot := store.Addr
	if storeFA, isFA := store.Addr.(*ssa.FieldAddr); isFA {
		storeRoot, path = fieldAddrPath(storeFA)
	}
	return path, storeRoot == root
}

// storeWritesFieldPath reports whether store writes the field that path selects
// from root: either that field itself, or a whole aggregate over a prefix of
// path.
func storeWritesFieldPath(store *ssa.Store, root ssa.Value, path []int) bool {
	storePath, ok := fieldStorePath(store, root)
	return ok && len(storePath) <= len(path) && slices.Equal(storePath, path[:len(storePath)])
}

// fieldStores returns, in program order, the stores that write the field that
// path selects from root. With a nil load it returns all of them. Otherwise it
// returns only the stores that reach load (#182): on some CFG path from the
// store to load, no other store writes the field.
//
//	t0 = &r.db
//	t1 = *t0                 // load: the store below does not reach it
//	t2 = Where(t1, "tenant")
//	t3 = &r.db
//	*t3 = t2
//	t4 = &r.db
//	t5 = *t4                 // load: reached by *t3 = t2 only
//
// Without this, the load on the right of `r.db = r.db.Where(...)` resolved to
// the value being stored, so the builder call counted as a branch of its own
// result. Only stores to the same (root, field path) are seen. A write through
// another pointer or inside a callee neither reaches the load nor replaces a
// store, as before.
func fieldStores(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction) []*ssa.Store {
	stores, _ := reachingFieldStores(fn, root, path, load)
	return stores
}

// reachingFieldStores is fieldStores that also reports whether the field can
// reach load unwritten: on some CFG path to load, no store writes it. A nil
// load is open, since it stands for a read that may run before any store.
//
// When root is a call, which defines the address, the walk stops at the call:
// a store above it on a loop path wrote the struct of an earlier iteration.
func reachingFieldStores(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction) (stores []*ssa.Store, open bool) {
	if load == nil {
		var all []*ssa.Store
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, root, path) {
					all = append(all, store)
				}
			}
		}
		return all, true
	}

	// lastIn returns the last store in instrs that writes the field.
	lastIn := func(instrs []ssa.Instruction) *ssa.Store {
		for _, instr := range slices.Backward(instrs) {
			if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, root, path) {
				return store
			}
		}
		return nil
	}

	// def is the call that defines root, and the walk ends there.
	def, _ := root.(*ssa.Call)
	// afterDef returns the instructions of block that can write root's field
	// before control leaves it, and whether def is among them.
	afterDef := func(instrs []ssa.Instruction) ([]ssa.Instruction, bool) {
		if def == nil {
			return instrs, false
		}
		if i := slices.Index(instrs, ssa.Instruction(def)); i >= 0 {
			return instrs[i+1:], true
		}
		return instrs, false
	}

	start := load.Block()
	prefix, defined := afterDef(start.Instrs[:slices.Index(start.Instrs, load)])
	if store := lastIn(prefix); store != nil {
		return []*ssa.Store{store}, false
	}
	if defined || len(start.Preds) == 0 {
		return nil, true
	}

	// Walk the CFG backward. A block that writes the field ends the walk on
	// that path with its last such store. The load's own block is scanned in
	// full when a loop leads back to it, since its later stores then run
	// before the load on the next iteration.
	var found []*ssa.Store
	visitedBlocks := make(map[*ssa.BasicBlock]bool)
	work := slices.Clone(start.Preds)
	for len(work) > 0 {
		block := work[len(work)-1]
		work = work[:len(work)-1]
		if visitedBlocks[block] {
			continue
		}
		visitedBlocks[block] = true
		instrs, defined := afterDef(block.Instrs)
		if store := lastIn(instrs); store != nil {
			found = append(found, store)
			continue
		}
		if defined || len(block.Preds) == 0 {
			open = true
			continue
		}
		work = append(work, block.Preds...)
	}
	// Each block contributes at most one store, so block order is program
	// order.
	slices.SortFunc(found, func(a, b *ssa.Store) int { return a.Block().Index - b.Block().Index })
	return found, open
}

// globalStoredValues returns, in program order, the values stored into the
// package-level variable g that reach load (#183). Only stores in load's own
// function are seen, matched as fieldStores matches a field. A load that no
// such store reaches has no stored value, so it is immutable: a global the
// function only reads usually holds a gorm.Open result, and treating every
// read of it as mutable would report each second use of a shared handle.
//
//	DB = DB.Where("tenant")  // the load on the right sees no store
//	DB.Find(nil)             // sees the store above
//
//declscope:shared // root.go's tracePointerLoad and traceAllPointerLoads resolve a global load with it
func globalStoredValues(g *ssa.Global, load ssa.Instruction) []ssa.Value {
	var vals []ssa.Value
	for _, store := range fieldStores(load.Parent(), g, nil, load) {
		vals = append(vals, store.Val)
	}
	return vals
}

//declscope:shared // root.go's traceAll and traceAllPointerLoads send local variables here
func (t *RootTracer) traceAllAllocStores(alloc *ssa.Alloc, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, v := range AllocStoredValues(alloc) {
		roots = append(roots, t.traceAll(v, visited, loopInfo)...)
	}
	return roots
}
