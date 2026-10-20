package tracer

import (
	"go/token"
	"slices"
	"strconv"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
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
// This function finds the Store instructions that write to the Alloc and can
// reach load (see allocReachingStoreValues). A nil load sees all of them.
//
//declscope:shared // root.go's traceNonCall and tracePointerLoad, and freevar.go's traceFreeVarLoad, send local variables here
func (t *RootTracer) traceAllocStore(alloc *ssa.Alloc, load ssa.Instruction, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	// Single-root: the first stored value that has a root (#340).
	if vals := t.stores.allocReachingStoreValues(alloc, load); len(vals) > 0 {
		return t.traceStores(vals, visited, loopInfo)
	}
	return nil
}

// allocReachingStoreValues returns, in program order, the values stored into
// alloc that a read at load can see (#251). A variable lives in an Alloc when a
// closure captures it and it is reassigned, so a read from it used to see the
// first store, often the parameter it was initialized with:
//
//	db = db.WithContext(ctx)   // *t0 = db (param); ...; *t0 = WithContext(...)
//	db.Find(nil)               // sees the WithContext store only
//	func() { db.Count(nil) }() // so does this read, at the IIFE's call
//
// The stores a read sees are those of observedFieldStores with an empty path.
// A nil load sees all of them. A store made inside a closure is not seen, as
// before.
func (m *storeMemo) allocReachingStoreValues(alloc *ssa.Alloc, load ssa.Instruction) []ssa.Value {
	fn := alloc.Parent()
	stores, _ := m.observedFieldStores(fn, storeTarget{root: alloc}, nil, load)
	vals := make([]ssa.Value, 0, len(stores))
	for _, store := range stores {
		vals = append(vals, store.Val)
	}
	return vals
}

// observedFieldStores is reachingFieldStores for a read at load, in fn or in a
// closure that fn creates, of an address that fn defines (#251, #303).
//
// A load in fn sees the stores that reach it. A load in a closure declared in
// fn whose only uses are plain calls sees the stores that reach those calls.
// Any other closure, deferred, spawned, stored, passed on, used as a
// range-over-func body, or nested deeper, runs at some point after the
// MakeClosure that fn runs for it, so it sees the stores that reach that
// MakeClosure and every store that can run after it:
//
//	db = db.WithContext(ctx)                // reaches the MakeClosure
//	g.Go(func() error { return db.... })    // sees WithContext, not the parameter
//	db = db.Where("x")                      // runs after it: seen too
//
// A load that no MakeClosure in fn leads to sees every store, as a nil load
// does.
//
// What every load of one closure sees is the same, so it is kept per closure
// (#325). Before, a closure called at n sites, with k loads of the address,
// walked back from every site for every load.
func (m *storeMemo) observedFieldStores(fn *ssa.Function, target storeTarget, path []int, load ssa.Instruction) (stores []*ssa.Store, open bool) {
	if load == nil || load.Parent() == fn {
		return m.reachingFieldStores(fn, target, path, load)
	}
	if m == nil {
		return m.closureObservedStores(fn, target, path, load.Parent())
	}
	key := observedStoreKey{storeQueryOf(fn, target, path), load.Parent()}
	if seen, ok := m.observed[key]; ok {
		return seen.stores, seen.open
	}
	stores, open = m.closureObservedStores(fn, target, path, load.Parent())
	m.observed[key] = storedReach{stores, open}
	return stores, open
}

// closureObservedStores is observedFieldStores for a load in closure, which
// is nested in fn.
func (m *storeMemo) closureObservedStores(fn *ssa.Function, target storeTarget, path []int, closure *ssa.Function) (stores []*ssa.Store, open bool) {
	mc := makeClosureInStoreFunc(fn, closure)
	if mc == nil {
		return m.reachingFieldStores(fn, target, path, nil)
	}
	var reads []ssa.Instruction
	if mc.Fn == ssa.Value(closure) {
		for _, call := range closureInvocationCalls(mc) {
			reads = append(reads, call)
		}
	}
	if len(reads) == 0 {
		reads = []ssa.Instruction{mc}
		stores = fieldStoresAfter(target, path, mc)
	}
	found := make(map[*ssa.Store]bool, len(stores))
	for _, store := range stores {
		found[store] = true
	}
	for _, read := range reads {
		reaching, readOpen := m.reachingFieldStores(fn, target, path, read)
		open = open || readOpen
		for _, store := range reaching {
			if !found[store] {
				found[store] = true
				stores = append(stores, store)
			}
		}
	}
	stores = slices.Clip(slices.Compact(sortStoresInProgramOrder(stores)))
	return stores, open
}

// makeClosureInStoreFunc returns the MakeClosure in fn that creates closure, or
// the closure that encloses it, or nil when closure is not nested in fn.
func makeClosureInStoreFunc(fn, closure *ssa.Function) *ssa.MakeClosure {
	for {
		mc := callsite.FreeVarMakeClosure(closure)
		if mc == nil || mc.Parent() == fn {
			return mc
		}
		closure = mc.Parent()
	}
}

// fieldStoresAfter returns the stores that write the field that path selects
// from target and can run after from: on some CFG path that leaves from. As in
// reachingFieldStores, a path ends at the call that defines the root, since a
// store past it writes the struct of a later iteration.
func fieldStoresAfter(target storeTarget, path []int, from ssa.Instruction) []*ssa.Store {
	def, _ := target.root.(*ssa.Call)
	var found []*ssa.Store
	// scan collects the stores in instrs and reports whether def ends the path.
	scan := func(instrs []ssa.Instruction) bool {
		for _, instr := range instrs {
			if def != nil && instr == ssa.Instruction(def) {
				return true
			}
			if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, target, path) && !slices.Contains(found, store) {
				found = append(found, store)
			}
		}
		return false
	}
	start := from.Block()
	if scan(start.Instrs[slices.Index(start.Instrs, from)+1:]) {
		return found
	}
	// The start block is scanned in full when a loop leads back to it, since
	// its stores before from then run after it.
	visitedBlocks := make(map[*ssa.BasicBlock]bool)
	work := slices.Clone(start.Succs)
	for len(work) > 0 {
		block := work[len(work)-1]
		work = work[:len(work)-1]
		if visitedBlocks[block] {
			continue
		}
		visitedBlocks[block] = true
		if !scan(block.Instrs) {
			work = append(work, block.Succs...)
		}
	}
	return found
}

// sortStoresInProgramOrder sorts stores by block, then by position in it.
func sortStoresInProgramOrder(stores []*ssa.Store) []*ssa.Store {
	slices.SortFunc(stores, func(a, b *ssa.Store) int {
		if a.Block() != b.Block() {
			return a.Block().Index - b.Block().Index
		}
		return slices.Index(a.Block().Instrs, ssa.Instruction(a)) - slices.Index(b.Block().Instrs, ssa.Instruction(b))
	})
	return stores
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
	var m *storeMemo
	return m.fieldAddrStoredValues(fa, load)
}

// fieldAddrStoredValues is FieldStoredValues, with the walks kept in m.
//
//declscope:shared // field.go's traceFieldStore and traceAllFieldStores resolve a field load with it
func (m *storeMemo) fieldAddrStoredValues(fa *ssa.FieldAddr, load ssa.Instruction) []ssa.Value {
	root, path := fieldAddrPath(fa)
	return m.fieldStoredValuesAt(fa.Parent(), root, path, load)
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
func (m *storeMemo) fieldValueStoredValues(f *ssa.Field) []ssa.Value {
	return m.structFieldStoredValues(f, nil, make(map[ssa.Value]bool))
}

// structFieldStoredValues returns the values of the field that path selects
// from the struct value x. seen stops Phi cycles through loops.
//
//declscope:shared // param_read.go selects the fields of a struct argument with it
func (m *storeMemo) structFieldStoredValues(x ssa.Value, path []int, seen map[ssa.Value]bool) []ssa.Value {
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
			vals = append(vals, m.structFieldStoredValues(edge, path, seen)...)
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
		return m.fieldStoredValuesAt(base.Parent(), root, append(prefix, path...), base)
	default:
		return nil
	}
}

// fieldStoredValuesAt returns the values stored into the field that path
// selects from root, as read by load in fn. Each address root may stand for
// (see fieldBaseAddrs) is searched in the function that defines it, or in fn
// for one without a function, such as a global. The stores returned are those
// that load observes (see observedFieldStores): a load in a closure of the
// defining function, reached through a captured pointer, sees the stores that
// reach the closure's calls or creation and those that run after it (#303).
//
//declscope:shared // param_read.go selects the fields of a pointer argument with it
func (m *storeMemo) fieldStoredValuesAt(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction) []ssa.Value {
	var vals []ssa.Value
	for _, base := range fieldBaseAddrs(root, make(map[ssa.Value]bool)) {
		baseFn := base.Parent()
		if baseFn == nil {
			baseFn = fn
		}
		vals = append(vals, m.fieldStoredValuesFrom(baseFn, base, path, load, make(map[storedFieldRef]bool))...)
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
func (m *storeMemo) fieldStoredValuesFrom(fn *ssa.Function, root ssa.Value, path []int, load ssa.Instruction, seen map[storedFieldRef]bool) []ssa.Value {
	ref := storedFieldRef{root: root, depth: len(path), load: load}
	if fn == nil || root == nil || len(path) == 0 || seen[ref] {
		return nil
	}
	seen[ref] = true

	target := newStoreTarget(root)
	stores, open := m.observedFieldStores(fn, target, path, load)
	var vals []ssa.Value
	if call, ok := root.(*ssa.Call); ok && open {
		// A field reached through a pointer that a call returned holds what
		// the call put there until a store replaces it (#250). That value is
		// the call's FieldRoot, as for a struct returned by value.
		vals = append(vals, &FieldRoot{Call: call, Path: slices.Clone(path)})
	}
	for _, store := range stores {
		storePath, _ := fieldStorePath(store, target)
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
				vals = append(vals, m.fieldStoredValuesFrom(fn, agg.X, path[len(storePath):], agg, seen)...)
			}
		case *ssa.Call:
			vals = append(vals, &FieldRoot{Call: agg, Path: slices.Clone(path[len(storePath):])})
		}
	}
	return vals
}

// storeTarget is the address whose stores a search matches: root, and, when
// root is the only pointer ever stored into the local variable slot, every
// load of slot (#303). A pointer variable that a closure captures lives in an
// Alloc, so each access through it loads it afresh:
//
//	t0 = new *deps (d)       // slot: d := &deps{db: db}, captured
//	t1 = new deps (complit)  // root
//	*t0 = t1
//	t3 = *t0
//	t8 = &t3.db
//	*t8 = t7                 // d.db = ...: writes root's db
//
// With more than one store in slot, a load of it may hold another pointer, so
// a store through it is not matched, as before.
type storeTarget struct {
	root ssa.Value
	slot *ssa.Alloc
}

// newStoreTarget returns the storeTarget of root.
func newStoreTarget(root ssa.Value) storeTarget {
	target := storeTarget{root: root}
	refs := root.Referrers()
	if refs == nil {
		return target
	}
	for _, ref := range *refs {
		store, ok := ref.(*ssa.Store)
		if !ok || store.Val != root {
			continue
		}
		if slot, ok := store.Addr.(*ssa.Alloc); ok && allocHasOneStore(slot) {
			target.slot = slot
			break
		}
	}
	return target
}

// allocHasOneStore reports whether exactly one store writes alloc.
func allocHasOneStore(alloc *ssa.Alloc) bool {
	n := 0
	for _, ref := range *alloc.Referrers() {
		if store, ok := ref.(*ssa.Store); ok && store.Addr == ssa.Value(alloc) {
			n++
		}
	}
	return n == 1
}

// holds reports whether addr is target's root.
func (target storeTarget) holds(addr ssa.Value) bool {
	if addr == target.root {
		return true
	}
	load, ok := addr.(*ssa.UnOp)
	return ok && target.slot != nil && load.Op == token.MUL && load.X == ssa.Value(target.slot)
}

// fieldStorePath returns the field path that store writes below target. ok is
// false when it writes somewhere else. A store to the root itself has an empty
// path.
func fieldStorePath(store *ssa.Store, target storeTarget) (path []int, ok bool) {
	storeRoot := store.Addr
	if storeFA, isFA := store.Addr.(*ssa.FieldAddr); isFA {
		storeRoot, path = fieldAddrPath(storeFA)
	}
	return path, target.holds(storeRoot)
}

// storeWritesFieldPath reports whether store writes the field that path selects
// from target: either that field itself, or a whole aggregate over a prefix of
// path.
func storeWritesFieldPath(store *ssa.Store, target storeTarget, path []int) bool {
	storePath, ok := fieldStorePath(store, target)
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
func (m *storeMemo) fieldStores(fn *ssa.Function, target storeTarget, path []int, load ssa.Instruction) []*ssa.Store {
	stores, _ := m.reachingFieldStores(fn, target, path, load)
	return stores
}

// reachingFieldStores is fieldStores that also reports whether the field can
// reach load unwritten: on some CFG path to load, no store writes it. A nil
// load is open, since it stands for a read that may run before any store.
//
// When root is a call, which defines the address, the walk stops at the call:
// a store above it on a loop path wrote the struct of an earlier iteration.
//
// Only the part of load's block before it is particular to load. What reaches
// the block's start is kept in m, per block (#325), so the loads of one block,
// or the n call sites of a closure each asked about by k loads, share a walk.
func (m *storeMemo) reachingFieldStores(fn *ssa.Function, target storeTarget, path []int, load ssa.Instruction) (stores []*ssa.Store, open bool) {
	if load == nil {
		return m.fieldStoresInto(fn, target, path, nil)
	}
	start := load.Block()
	instrs, defined := fieldStoreDefTail(target, start.Instrs[:slices.Index(start.Instrs, load)])
	if store := lastFieldStore(target, path, instrs); store != nil {
		return []*ssa.Store{store}, false
	}
	if defined {
		return nil, true
	}
	return m.fieldStoresInto(fn, target, path, start)
}

// fieldStoresInto returns reachingFieldStores for a read at the start of
// block, or for a nil load when block is nil. The result is kept in m.
func (m *storeMemo) fieldStoresInto(fn *ssa.Function, target storeTarget, path []int, block *ssa.BasicBlock) (stores []*ssa.Store, open bool) {
	if m == nil {
		return searchFieldStoresInto(fn, target, path, block)
	}
	key := reachingStoreKey{storeQueryOf(fn, target, path), block}
	if seen, ok := m.reaching[key]; ok {
		return seen.stores, seen.open
	}
	stores, open = searchFieldStoresInto(fn, target, path, block)
	m.reaching[key] = storedReach{stores, open}
	return stores, open
}

// searchFieldStoresInto is fieldStoresInto without the memo.
func searchFieldStoresInto(fn *ssa.Function, target storeTarget, path []int, block *ssa.BasicBlock) ([]*ssa.Store, bool) {
	if block == nil {
		return slices.Clip(allFieldStores(fn, target, path)), true
	}
	stores, open := walkFieldStoresBack(target, path, block)
	return slices.Clip(stores), open
}

// allFieldStores returns, in program order, every store in fn that writes the
// field that path selects from target.
func allFieldStores(fn *ssa.Function, target storeTarget, path []int) []*ssa.Store {
	var all []*ssa.Store
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, target, path) {
				all = append(all, store)
			}
		}
	}
	return all
}

// lastFieldStore returns the last store in instrs that writes the field.
func lastFieldStore(target storeTarget, path []int, instrs []ssa.Instruction) *ssa.Store {
	for _, instr := range slices.Backward(instrs) {
		if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, target, path) {
			return store
		}
	}
	return nil
}

// fieldStoreDefTail returns the part of instrs that can write the field of
// target's root before control leaves it, and whether the call that defines
// the root is in instrs. The walk ends at that call.
func fieldStoreDefTail(target storeTarget, instrs []ssa.Instruction) ([]ssa.Instruction, bool) {
	def, ok := target.root.(*ssa.Call)
	if !ok {
		return instrs, false
	}
	if i := slices.Index(instrs, ssa.Instruction(def)); i >= 0 {
		return instrs[i+1:], true
	}
	return instrs, false
}

// walkFieldStoresBack returns the stores that reach the start of block, and
// whether the field reaches it unwritten.
//
// It walks the CFG backward. A block that writes the field ends the walk on
// that path with its last such store. block itself is scanned in full when a
// loop leads back to it, since its stores then run before its start on the
// next iteration.
func walkFieldStoresBack(target storeTarget, path []int, block *ssa.BasicBlock) (found []*ssa.Store, open bool) {
	if len(block.Preds) == 0 {
		return nil, true
	}
	visitedBlocks := make(map[*ssa.BasicBlock]bool)
	work := slices.Clone(block.Preds)
	for len(work) > 0 {
		block := work[len(work)-1]
		work = work[:len(work)-1]
		if visitedBlocks[block] {
			continue
		}
		visitedBlocks[block] = true
		instrs, defined := fieldStoreDefTail(target, block.Instrs)
		if store := lastFieldStore(target, path, instrs); store != nil {
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

// storeMemo keeps the stores that reads of an address see (#325). A nil
// storeMemo keeps nothing, for the callers outside a tracer.
//
// Each entry depends only on the SSA of the function asked about, so a tracer
// shares one memo across all the functions it traces.
//
//declscope:shared // root.go's RootTracer holds one, and its methods resolve loads through it
type storeMemo struct {
	//declscope:private
	reaching map[reachingStoreKey]storedReach // What reaches the start of each block, or every store for a nil block
	//declscope:private
	observed map[observedStoreKey]storedReach // What the loads in each closure see
}

// newStoreMemo returns an empty storeMemo.
//
//declscope:shared // root.go's NewRootTracer gives each tracer one
func newStoreMemo() *storeMemo {
	return &storeMemo{
		reaching: make(map[reachingStoreKey]storedReach),
		observed: make(map[observedStoreKey]storedReach),
	}
}

// storeQuery is the field that path selects from target, as searched in fn.
type storeQuery struct {
	fn   *ssa.Function
	root ssa.Value
	slot *ssa.Alloc
	path string
}

// storeQueryOf returns the storeQuery of a search.
func storeQueryOf(fn *ssa.Function, target storeTarget, path []int) storeQuery {
	key := make([]byte, 0, 4*len(path))
	for _, i := range path {
		key = strconv.AppendInt(key, int64(i), 10)
		key = append(key, '.')
	}
	return storeQuery{fn: fn, root: target.root, slot: target.slot, path: string(key)}
}

// reachingStoreKey is a fieldStoresInto search.
type reachingStoreKey struct {
	storeQuery
	block *ssa.BasicBlock
}

// observedStoreKey is an observedFieldStores search for the loads in closure.
type observedStoreKey struct {
	storeQuery
	closure *ssa.Function
}

// storedReach is the result of a search: the stores, in program order, and
// whether the field can be read unwritten.
type storedReach struct {
	stores []*ssa.Store
	open   bool
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
func (m *storeMemo) globalStoredValues(g *ssa.Global, load ssa.Instruction) []ssa.Value {
	var vals []ssa.Value
	for _, store := range m.fieldStores(load.Parent(), storeTarget{root: g}, nil, load) {
		vals = append(vals, store.Val)
	}
	return vals
}

// traceAllAllocStores is traceAllocStore for every value that can reach load.
//
//declscope:shared // root.go's traceAll and traceAllPointerLoads, and freevar.go's traceAllFreeVarLoads, send local variables here
func (t *RootTracer) traceAllAllocStores(alloc *ssa.Alloc, load ssa.Instruction, search *allRootsSearch, loopInfo *cfg.LoopInfo) []ssa.Value {
	var roots []ssa.Value
	for _, v := range t.stores.allocReachingStoreValues(alloc, load) {
		roots = append(roots, t.traceAll(v, search, loopInfo)...)
	}
	return roots
}

// traceStores returns the root of the first of vals that has one, as tracePhi
// does for the edges of a merge. vals are the values a variable in memory may
// hold: the stores that reach a read of it. Stopping at the first store lost
// every root after an immutable one, such as WithContext before a conditional
// Where (#340).
//
//declscope:shared // root.go and field.go trace the stores of fields and globals with it too
func (t *RootTracer) traceStores(vals []ssa.Value, visited map[ssa.Value]bool, loopInfo *cfg.LoopInfo) ssa.Value {
	for _, v := range vals {
		if root := t.trace(v, visited, loopInfo); root != nil {
			return root
		}
	}
	return nil
}
