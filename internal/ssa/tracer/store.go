package tracer

import (
	"cmp"
	"go/token"
	"slices"
	"strconv"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/effort"
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
	// Single-root: the first stored value that has a root (#340). What a call
	// that takes the variable's address may store comes after them (#345).
	if vals := append(t.stores.allocReachingStoreValues(alloc, load), t.pointeeRootsAt(alloc, load)...); len(vals) > 0 {
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
// Any other closure, deferred, spawned, stored, passed on, or nested deeper,
// runs at some point after the MakeClosure that fn runs for it, so it sees the
// stores that reach that MakeClosure and every store that can run after it:
//
//	db = db.WithContext(ctx)                // reaches the MakeClosure
//	g.Go(func() error { return db.... })    // sees WithContext, not the parameter
//	db = db.Where("x")                      // runs after it: seen too
//
// A range-over-func body, and a closure nested only in such bodies, runs only
// while its loop statement does, so it sees only the stores that reach the
// MakeClosure (#341, see missesLaterStores). A read in a body does not see
// the body's own stores, as before.
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
		if !missesLaterStores(closure, fn) {
			stores = m.fieldStoresAfter(target, path, mc)
		}
	}
	if len(reads) == 1 && len(stores) == 0 {
		// The stores of one read are in program order already.
		return m.reachingFieldStores(fn, target, path, reads[0])
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
	stores = slices.Clip(sortStoresInProgramOrder(fn, stores))
	return stores, open
}

// missesLaterStores reports whether closure cannot see a store made after its
// MakeClosure in fn: it is a range-over-func body in fn, or nested in fn only
// through such bodies (#341). It runs only while the loop
// statement runs, and go/ssa panics on a call of it after the statement ends,
// so no store after the statement reaches it. A store on a later iteration of
// a loop around the statement reaches the MakeClosure on the back-edge.
func missesLaterStores(closure, fn *ssa.Function) bool {
	for f := closure; f != fn; f = f.Parent() {
		if !cfg.IsRangeFuncLoopBody(f) {
			return false
		}
	}
	return true
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
// store past it writes the struct of a later iteration. The stores of a
// range-over-func body run where the loop does (see yieldStores). from is
// never the MakeClosure of such a body (see missesLaterStores).
func (m *storeMemo) fieldStoresAfter(target storeTarget, path []int, from ssa.Instruction) []*ssa.Store {
	def, _ := target.root.(*ssa.Call)
	var found []*ssa.Store
	add := func(store *ssa.Store) {
		if !slices.Contains(found, store) {
			found = append(found, store)
		}
	}
	// scan collects the stores in instrs and reports whether def ends the path.
	scan := func(instrs []ssa.Instruction) bool {
		for _, instr := range instrs {
			if def != nil && instr == ssa.Instruction(def) {
				return true
			}
			if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, target, path) {
				add(store)
			}
			for _, store := range m.yieldStores(target, path, instr) {
				add(store)
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

// sortStoresInProgramOrder sorts stores, which are distinct, by block of fn,
// then by position in it. A store in a range-over-func body is placed at the
// MakeClosure in fn that creates the body (see yieldStores), and the stores
// placed there keep their order.
func sortStoresInProgramOrder(fn *ssa.Function, stores []*ssa.Store) []*ssa.Store {
	// The MakeClosure in fn of each closure that fn creates, found on the
	// first store outside fn.
	var closures map[*ssa.Function]*ssa.MakeClosure
	anchor := func(store *ssa.Store) ssa.Instruction {
		f := store.Parent()
		if f == fn {
			return store
		}
		for f.Parent() != fn {
			f = f.Parent()
		}
		if closures == nil {
			closures = make(map[*ssa.Function]*ssa.MakeClosure)
			for _, block := range fn.Blocks {
				for _, instr := range block.Instrs {
					if mc, ok := instr.(*ssa.MakeClosure); ok {
						closures[mc.Fn.(*ssa.Function)] = mc
					}
				}
			}
		}
		return closures[f]
	}
	type keyed struct {
		store        *ssa.Store
		block, index int
	}
	keys := make([]keyed, len(stores))
	for i, store := range stores {
		at := anchor(store)
		keys[i] = keyed{store, at.Block().Index, slices.Index(at.Block().Instrs, at)}
	}
	slices.SortStableFunc(keys, func(a, b keyed) int {
		if a.block != b.block {
			return a.block - b.block
		}
		return a.index - b.index
	})
	for i, k := range keys {
		stores[i] = k.store
	}
	return stores
}

// yieldStores returns, in program order, the stores that write the field that
// path selects from target in the range-over-func body that instr creates,
// when instr is the MakeClosure of one (#341). The body reaches target
// through its FreeVars (see storeTarget.inBody). The stores of a loop nested
// in the body are included, where its MakeClosure is.
//
// A body runs only while its loop statement runs: the iterator calls it right
// after the MakeClosure, and a call after the loop ends panics. So its stores
// are taken as stores at the MakeClosure. A body may run no time, so they do
// not replace what was stored before:
//
//	q := db.WithContext(ctx)
//	for k, v := range maps.All(filters) {
//		q = q.Where(k+" = ?", v) // a store to q's FreeVar in the body
//	}
//	q.Find(&users)               // sees WithContext and the Where
//
// Before #341 a read after the loop saw only WithContext, so a reuse of the
// Where was missed. A store in any other closure is still not seen.
//
// The stores of each body are kept in m, so a walk that passes n loops does
// not scan their bodies again: a read after n loops, each in a loop body,
// scanned the earlier bodies once per read.
func (m *storeMemo) yieldStores(target storeTarget, path []int, instr ssa.Instruction) []*ssa.Store {
	mc, ok := instr.(*ssa.MakeClosure)
	if !ok {
		return nil
	}
	body, ok := mc.Fn.(*ssa.Function)
	if !ok || !cfg.IsRangeFuncLoopBody(body) {
		return nil
	}
	if m == nil {
		return m.searchYieldStores(target, path, mc, body)
	}
	key := yieldStoreKey{storeQueryOf(nil, target, path), mc}
	found, ok := m.yields[key]
	if !ok {
		found = m.searchYieldStores(target, path, mc, body)
		m.yields[key] = found
	}
	return found
}

// searchYieldStores is yieldStores without the memo, for the body that mc
// creates.
func (m *storeMemo) searchYieldStores(target storeTarget, path []int, mc *ssa.MakeClosure, body *ssa.Function) []*ssa.Store {
	inner, ok := target.inBody(mc, body)
	if !ok {
		return nil
	}
	var found []*ssa.Store
	for _, block := range body.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, inner, path) {
				found = append(found, store)
			}
			found = append(found, m.yieldStores(inner, path, instr)...)
		}
	}
	return slices.Clip(found)
}

// AllocStoredValues returns, in program order, the values stored into alloc.
// Shared by traceAllocStore (first), traceAllAllocStores (all), the Scopes
// callback resolver, and scopeswarn. It reads the referrers of alloc, not
// every instruction of its function, so a function with n such variables is
// not scanned n times (#350).
func AllocStoredValues(alloc *ssa.Alloc) []ssa.Value {
	fn := alloc.Parent()
	if fn == nil || alloc.Referrers() == nil {
		return nil
	}
	type indexed struct {
		block, instr int
		val          ssa.Value
	}
	var stores []indexed
	for _, ref := range *alloc.Referrers() {
		// A store in a block that SSA removed as unreachable is not one.
		store, ok := ref.(*ssa.Store)
		if !ok || store.Addr != ssa.Value(alloc) || !storeBlockInFunction(fn, store.Block()) {
			continue
		}
		stores = append(stores, indexed{store.Block().Index, slices.Index(store.Block().Instrs, ssa.Instruction(store)), store.Val})
	}
	slices.SortFunc(stores, func(a, b indexed) int {
		return cmp.Or(cmp.Compare(a.block, b.block), cmp.Compare(a.instr, b.instr))
	})
	vals := make([]ssa.Value, 0, len(stores))
	for _, store := range stores {
		vals = append(vals, store.val)
	}
	return vals
}

// storeBlockInFunction reports whether block is one of fn's blocks.
func storeBlockInFunction(fn *ssa.Function, block *ssa.BasicBlock) bool {
	return block != nil && block.Index < len(fn.Blocks) && fn.Blocks[block.Index] == block
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
//declscope:shared // param_read.go selects the fields of a struct argument with it, and field.go's GormDBFieldValues those of a struct handed to a call
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
//declscope:shared // param_read.go selects the fields of a pointer argument with it, and field.go's GormDBFieldValues those of a struct handed to a call
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
				// The copy may be in a range-over-func body (yieldStores).
				vals = append(vals, m.fieldStoredValuesFrom(agg.Parent(), agg.X, path[len(storePath):], agg, seen)...)
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
//
// In a range-over-func body, both are the FreeVars that the body reads them
// through (see inBody), so slot is not always an Alloc.
type storeTarget struct {
	root ssa.Value
	slot ssa.Value
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
	return ok && target.slot != nil && load.Op == token.MUL && load.X == target.slot
}

// inBody returns target as the range-over-func body that mc creates reaches
// it: through the FreeVar of each address that mc binds. A package-level
// variable is reached directly. ok is false when the body reaches neither
// address.
func (target storeTarget) inBody(mc *ssa.MakeClosure, body *ssa.Function) (inner storeTarget, ok bool) {
	if _, global := target.root.(*ssa.Global); global {
		inner.root = target.root
	}
	for i, binding := range mc.Bindings {
		switch binding {
		case target.root:
			inner.root = body.FreeVars[i]
		case target.slot:
			inner.slot = body.FreeVars[i]
		}
	}
	return inner, inner.root != nil || inner.slot != nil
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
	last, written := m.appendLastFieldStores(nil, target, path, instrs)
	switch {
	case written:
		return last, false
	case defined:
		return last, true
	}
	before, open := m.fieldStoresInto(fn, target, path, start)
	if len(last) == 0 {
		return before, open
	}
	// A loop back to start already put start's stores in before.
	stores = slices.Clone(before)
	for _, store := range last {
		if !slices.Contains(before, store) {
			stores = append(stores, store)
		}
	}
	return stores, open
}

// fieldStoresInto returns reachingFieldStores for a read at the start of
// block, or for a nil load when block is nil. The result is kept in m.
func (m *storeMemo) fieldStoresInto(fn *ssa.Function, target storeTarget, path []int, block *ssa.BasicBlock) (stores []*ssa.Store, open bool) {
	if m == nil {
		return m.searchFieldStoresInto(fn, target, path, block)
	}
	key := reachingStoreKey{storeQueryOf(fn, target, path), block}
	if seen, ok := m.reaching[key]; ok {
		return seen.stores, seen.open
	}
	stores, open = m.searchFieldStoresInto(fn, target, path, block)
	m.reaching[key] = storedReach{stores, open}
	return stores, open
}

// searchFieldStoresInto is fieldStoresInto without the memo.
func (m *storeMemo) searchFieldStoresInto(fn *ssa.Function, target storeTarget, path []int, block *ssa.BasicBlock) ([]*ssa.Store, bool) {
	if block == nil {
		return slices.Clip(m.allFieldStores(fn, target, path)), true
	}
	stores, open := m.walkFieldStoresBack(target, path, block)
	return slices.Clip(stores), open
}

// allFieldStores returns, in program order, every store in fn that writes the
// field that path selects from target, with those of its range-over-func
// bodies (see yieldStores).
func (m *storeMemo) allFieldStores(fn *ssa.Function, target storeTarget, path []int) []*ssa.Store {
	var all []*ssa.Store
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, target, path) {
				all = append(all, store)
			}
			all = append(all, m.yieldStores(target, path, instr)...)
		}
	}
	return all
}

// appendLastFieldStores appends to dst, in program order, the stores in
// instrs that the end of instrs sees: the last store that writes the field,
// and the stores of each range-over-func body after it (see yieldStores).
// written reports whether there is such a last store. A body may run no time,
// so its stores do not end the search.
func (m *storeMemo) appendLastFieldStores(dst []*ssa.Store, target storeTarget, path []int, instrs []ssa.Instruction) (stores []*ssa.Store, written bool) {
	start := len(dst)
	for _, instr := range slices.Backward(instrs) {
		if store, ok := instr.(*ssa.Store); ok && storeWritesFieldPath(store, target, path) {
			dst, written = append(dst, store), true
			break
		}
		for _, store := range slices.Backward(m.yieldStores(target, path, instr)) {
			dst = append(dst, store)
		}
	}
	slices.Reverse(dst[start:])
	return dst, written
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
func (m *storeMemo) walkFieldStoresBack(target storeTarget, path []int, block *ssa.BasicBlock) (found []*ssa.Store, open bool) {
	if len(block.Preds) == 0 {
		return nil, true
	}
	visitedBlocks := make(map[*ssa.BasicBlock]bool)
	work := slices.Clone(block.Preds)
	var spans []blockStores
	for len(work) > 0 {
		block := work[len(work)-1]
		work = work[:len(work)-1]
		if visitedBlocks[block] {
			continue
		}
		visitedBlocks[block] = true
		effort.Step()
		instrs, defined := fieldStoreDefTail(target, block.Instrs)
		start := len(found)
		var written bool
		found, written = m.appendLastFieldStores(found, target, path, instrs)
		if len(found) > start {
			spans = append(spans, blockStores{block.Index, start, len(found)})
		}
		switch {
		case written:
			continue
		case defined || len(block.Preds) == 0:
			open = true
			continue
		}
		work = append(work, block.Preds...)
	}
	// Each block's stores are in program order, so block order makes the
	// whole program order.
	if slices.IsSortedFunc(spans, compareBlockStores) {
		return found, open
	}
	slices.SortFunc(spans, compareBlockStores)
	ordered := make([]*ssa.Store, 0, len(found))
	for _, b := range spans {
		ordered = append(ordered, found[b.start:b.end]...)
	}
	return ordered, open
}

// blockStores is the part found[start:end] of what walkFieldStoresBack finds
// that the block with this index holds.
type blockStores struct {
	index, start, end int
}

// compareBlockStores orders blockStores by block.
func compareBlockStores(a, b blockStores) int { return a.index - b.index }

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
	//declscope:private
	yields map[yieldStoreKey][]*ssa.Store // The stores of each range-over-func body (#341)
}

// newStoreMemo returns an empty storeMemo.
//
//declscope:shared // root.go's NewRootTracer gives each tracer one
func newStoreMemo() *storeMemo {
	return &storeMemo{
		reaching: make(map[reachingStoreKey]storedReach),
		observed: make(map[observedStoreKey]storedReach),
		yields:   make(map[yieldStoreKey][]*ssa.Store),
	}
}

// storeQuery is the field that path selects from target, as searched in fn.
type storeQuery struct {
	fn   *ssa.Function
	root ssa.Value
	slot ssa.Value
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

// yieldStoreKey is a yieldStores search in the body that mc creates. Its fn
// is nil: the body, reached through mc, defines where it searches.
type yieldStoreKey struct {
	storeQuery
	mc *ssa.MakeClosure
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
	for _, v := range t.pointeeRootsAt(alloc, load) {
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
