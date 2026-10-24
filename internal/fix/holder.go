package fix

import (
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// holderCache memoizes, for the whole pass, what holderChangesRoot asks of
// each function: the stores of a *gorm.DB into a struct field, the
// instructions that hand on each local struct, and the calls that may change
// memory other functions reach (#310). Each is built once per function or
// struct, so a function with many such stores stays linear.
//
//declscope:shared // generator.go's Generator holds it for the pass
type holderCache struct {
	//declscope:private
	stores map[*ssa.Function]map[ssa.Value][]*ssa.Store
	//declscope:private
	walks map[*ssa.Alloc]*holderWalk
	//declscope:private
	calls map[*ssa.Function]*holderCalls
	//declscope:private
	gorm map[*types.Package]bool
	//declscope:private
	runs map[types.Type]bool
}

// holderChangesRoot reports whether a store of v's root into a struct field,
// in its function or a closure of it, lets other code change the root in
// place where a use may see it (see holderStoreChangesRoot, #310).
//
//declscope:shared // change.go's changedElsewhere withholds the fix with it
func (g *Generator) holderChangesRoot(v pollution.Violation, ordered bool) bool {
	for _, f := range withClosures(v.Root.Parent()) {
		if slices.ContainsFunc(g.holderStores(f)[v.Root], func(store *ssa.Store) bool {
			return g.holderStoreChangesRoot(v, store, ordered)
		}) {
			return true
		}
	}
	return false
}

// holderStoreChangesRoot reports whether store, which puts v's root or a chain
// on it into a struct field (see holderStores), lets other code change the
// root in place where a use may see it (#310). The store is no use of the
// root: detection reads the fields of a local struct as the value stored in
// them (#181). Other code reaches the root where it reaches the struct, and
// each such point is an escape for changeSeenAfterEscape, as a call argument
// is:
//   - A local struct is reached where it is handed on (see holderWalk), such
//     as `s := &Svc{db: q}; s.apply()` or `applyP(params{db: q})`.
//   - Other memory, such as a package-level variable or a struct a parameter
//     points to, is reached by the calls that may run after the store, other
//     than *gorm.DB methods (see holderCallChanges). The first call after it
//     in its block is the escape. A call in a later block is taken as running
//     at the end of the store's block. A local struct copied into such
//     memory is reached the same way after the copy.
//
// A //gormreuse:pure function changes nothing it is handed as an argument, as
// in paDeref(&q). Its body check does not cover the receiver, so a pure method
// may change what its receiver holds (#404). A pure call is therefore an escape
// only where the struct reaches it as the receiver, or where the struct is in
// other memory, which the receiver may reach. Code in a closure is not ordered
// against the uses, so an escape there, or a store there into memory that is
// not a local struct, is taken as seen.
func (g *Generator) holderStoreChangesRoot(v pollution.Violation, store *ssa.Store, ordered bool) bool {
	fn := v.Root.Parent()
	def := tracer.RootBlock(v.Root)
	// seen reports whether a use may see a change made where instr reaches
	// the struct through via. via is nil for other memory.
	seen := func(instr ssa.Instruction, via ssa.Value) bool {
		if call, ok := instr.(ssa.CallInstruction); ok {
			if site := g.tracer.Site(call); !site.IsGormMethod() && g.tracer.IsPureFunction(site.Declared) &&
				(site.Recv == nil || via != nil && via != site.Recv) {
				return false
			}
		}
		if instr.Parent() != fn || !instr.Pos().IsValid() {
			return true
		}
		use := pollution.UsageInfo{Block: instr.Block(), Pos: instr.Pos(), Escapes: true}
		return changeSeenAfterEscape(g.cfg, def, use, v.AllUses, ordered, g.tracer.FuncVars())
	}
	reached := func(at *ssa.Store) bool {
		if at.Parent() != fn {
			return true
		}
		next, later := g.holderNextCall(at)
		return next != nil && seen(next, nil) || later && holderChangeSeenAfterBlock(g.cfg, def, at.Block(), v.AllUses, ordered)
	}
	alloc, ok := holderBase(store.Addr)
	if !ok {
		return reached(store)
	}
	h := g.holderEscapes(alloc)
	return slices.ContainsFunc(h.escapes, func(e holderEscape) bool { return seen(e.instr, e.via) }) ||
		slices.ContainsFunc(h.copies, reached)
}

// holderChangeSeenAfterBlock reports whether a use may see a change made at
// the end of block: a use in a block it reaches, or any use when block runs
// again or the uses have no order (see changeSeenAfterEscape).
func holderChangeSeenAfterBlock(a *cfg.Analyzer, def, block *ssa.BasicBlock, uses []pollution.UsageInfo, ordered bool) bool {
	if !ordered || useRepeatsAfter(a, block, def) {
		return true
	}
	return slices.ContainsFunc(uses, func(use pollution.UsageInfo) bool {
		return use.Block != block && a.CanReach(block, use.Block)
	})
}

// holderStores returns the stores in f of a *gorm.DB into a struct field, or
// into anything but a slice, array or map element, keyed by each root whose
// changes the stored value carries (changeCarryingRoots). A store into a local
// variable counts too: a variable lives in memory when its address is taken,
// and code its address is handed to, as in applyP(&q), reaches the root
// (#345). holderWalk finds where it is handed on. A variable that is only
// read and written, also by the closures that capture it, hands nothing on.
func (g *Generator) holderStores(f *ssa.Function) map[ssa.Value][]*ssa.Store {
	c := &g.holders
	if stores, ok := c.stores[f]; ok {
		return stores
	}
	if c.stores == nil {
		c.stores = map[*ssa.Function]map[ssa.Value][]*ssa.Store{}
	}
	stores := map[ssa.Value][]*ssa.Store{}
	loopInfo := g.cfg.DetectLoops(f)
	for _, block := range f.Blocks {
		for _, instr := range block.Instrs {
			store, ok := instr.(*ssa.Store)
			if !ok {
				continue
			}
			switch store.Addr.(type) {
			case *ssa.FreeVar, *ssa.IndexAddr:
				// A variable a closure captures, which its function walks,
				// or a pollutionsource.Leak element.
				continue
			}
			val, ok := pollutionsource.UnwrapGormDB(store.Val)
			if !ok {
				continue
			}
			for _, root := range g.changeCarryingRoots(val, loopInfo) {
				stores[root] = append(stores[root], store)
			}
		}
	}
	c.stores[f] = stores
	return stores
}

// holderEscapes returns the walk of the local struct that alloc builds (see
// holderWalk).
func (g *Generator) holderEscapes(alloc *ssa.Alloc) *holderWalk {
	c := &g.holders
	if h, ok := c.walks[alloc]; ok {
		return h
	}
	if c.walks == nil {
		c.walks = map[*ssa.Alloc]*holderWalk{}
	}
	h := &holderWalk{seen: map[ssa.Value]bool{}}
	h.addr(alloc)
	h.seen = nil
	c.walks[alloc] = h
	return h
}

// holderBase returns the local struct or array that addr, its address or the
// address of a field or element in it, is in. It returns false for other
// memory, such as a package-level variable, a slice element or a struct a
// pointer points to.
func holderBase(addr ssa.Value) (*ssa.Alloc, bool) {
	for {
		switch a := addr.(type) {
		case *ssa.FieldAddr:
			addr = a.X
			continue
		case *ssa.IndexAddr:
			if _, array := a.X.Type().Underlying().(*types.Pointer); array {
				addr = a.X
				continue
			}
		}
		alloc, ok := addr.(*ssa.Alloc)
		return alloc, ok
	}
}

// holderWalk collects the instructions that hand on a local struct: a call
// that takes it, also as a receiver, or a closure that captures it and does
// so. A store of a copy of it, or of its address, into memory that is not a
// local struct or variable, such as a package-level variable, is collected
// as a copy: other functions reach it from there. It follows
// the addresses of its fields, the copies of its value, the variables that
// hold its address, and the closures that capture either. A read of a
// *gorm.DB field hands nothing on: detection reads it as the value stored in
// it (#181).
type holderWalk struct {
	seen    map[ssa.Value]bool
	escapes []holderEscape
	copies  []*ssa.Store
}

// holderEscape is an instruction that hands on a local struct, and via, the
// operand through which it reaches the struct.
type holderEscape struct {
	instr ssa.Instruction
	via   ssa.Value
}

// addr walks the uses of addr, the address of the struct or of a part of it.
// Its field and element addresses, a Phi that merges it, and the values
// loaded from it are walked too, and so is a closure that captures it. A
// store into it hands nothing on. A store of it into a local variable is
// walked through that variable.
func (h *holderWalk) addr(addr ssa.Value) {
	if h.seen[addr] {
		return
	}
	h.seen[addr] = true
	for _, r := range *addr.Referrers() {
		switch r := r.(type) {
		case *ssa.FieldAddr, *ssa.IndexAddr, *ssa.Phi:
			h.addr(r.(ssa.Value))
		case *ssa.UnOp:
			h.value(r)
		case *ssa.Store:
			if r.Val == addr {
				h.pointerVar(r, r.Addr)
			}
		case *ssa.MakeClosure:
			h.captured(r, addr, h.addr)
		case *ssa.DebugRef:
		default:
			h.escapes = append(h.escapes, holderEscape{r, addr})
		}
	}
}

// pointerVar walks v, a variable that store wrote the struct's address into,
// such as s in `s := &Svc{db: q}` when a closure captures s. A load of it is
// the struct's address again. Memory that is not a local variable is a copy
// at store.
func (h *holderWalk) pointerVar(store *ssa.Store, v ssa.Value) {
	switch v.(type) {
	case *ssa.Alloc, *ssa.FreeVar:
	default:
		h.copies = append(h.copies, store)
		return
	}
	if h.seen[v] {
		return
	}
	h.seen[v] = true
	for _, r := range *v.Referrers() {
		switch r := r.(type) {
		case *ssa.UnOp:
			h.addr(r)
		case *ssa.Store:
			if r.Val == v {
				h.escapes = append(h.escapes, holderEscape{r, v})
			}
		case *ssa.MakeClosure:
			h.captured(r, v, func(fv ssa.Value) { h.pointerVar(store, fv) })
		case *ssa.DebugRef:
		default:
			h.escapes = append(h.escapes, holderEscape{r, v})
		}
	}
}

// captured walks, with walk, the free variable of the closure that mc makes
// for each binding of v.
func (h *holderWalk) captured(mc *ssa.MakeClosure, v ssa.Value, walk func(ssa.Value)) {
	fn := mc.Fn.(*ssa.Function)
	for i, b := range mc.Bindings {
		if b == v {
			walk(fn.FreeVars[i])
		}
	}
}

// value walks the uses of v, a value loaded from the struct, taken from a
// field of such a value, or merged from such values by a Phi. A *gorm.DB is
// the root itself to detection. A value that holds no struct or array inline,
// such as a pointer or an int, holds nothing the struct stores inline. A copy
// stored into another local struct is walked there. One stored anywhere else
// is a copy.
func (h *holderWalk) value(v ssa.Value) {
	if typeutil.IsGormDB(v.Type()) || !holderInlineType(v.Type()) || h.seen[v] {
		return
	}
	h.seen[v] = true
	for _, r := range *v.Referrers() {
		switch r := r.(type) {
		case *ssa.Field, *ssa.Phi:
			h.value(r.(ssa.Value))
		case *ssa.Store:
			if alloc, ok := holderBase(r.Addr); ok {
				h.addr(alloc)
			} else {
				h.copies = append(h.copies, r)
			}
		case *ssa.DebugRef:
		default:
			h.escapes = append(h.escapes, holderEscape{r, v})
		}
	}
}

// holderInlineType reports whether a value of type t holds struct fields or
// array elements inline: a struct or an array.
func holderInlineType(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Struct, *types.Array:
		return true
	}
	return false
}

// holderCalls lists the calls of a function that may change memory other
// functions reach (see holderCallChanges), in the order each block runs
// them, and the blocks from which such a call can still run.
type holderCalls struct {
	inBlock map[*ssa.BasicBlock][]ssa.Instruction
	index   map[ssa.Instruction]int  // an instruction's index in its block
	from    map[*ssa.BasicBlock]bool // the block, or one it reaches, has such a call
}

// holderFuncCalls returns the calls of f that may change memory other
// functions reach.
func (g *Generator) holderFuncCalls(f *ssa.Function) *holderCalls {
	c := &g.holders
	if calls, ok := c.calls[f]; ok {
		return calls
	}
	if c.calls == nil {
		c.calls = map[*ssa.Function]*holderCalls{}
	}
	calls := &holderCalls{inBlock: map[*ssa.BasicBlock][]ssa.Instruction{}, index: map[ssa.Instruction]int{}, from: map[*ssa.BasicBlock]bool{}}
	var work []*ssa.BasicBlock
	for _, block := range f.Blocks {
		for i, instr := range block.Instrs {
			calls.index[instr] = i
			if g.holderCallChanges(instr) {
				calls.inBlock[block] = append(calls.inBlock[block], instr)
			}
		}
		if len(calls.inBlock[block]) > 0 {
			calls.from[block] = true
			work = append(work, block)
		}
	}
	for len(work) > 0 {
		block := work[len(work)-1]
		work = work[:len(work)-1]
		for _, pred := range block.Preds {
			if !calls.from[pred] {
				calls.from[pred] = true
				work = append(work, pred)
			}
		}
	}
	c.calls[f] = calls
	return calls
}

// holderCallChanges reports whether instr may run code that changes memory
// other functions reach before the function returns. A builtin does not,
// other than close, nor does a deferred call, which runs after every use.
// Neither does a call into a package that does not reach gorm, when no
// argument can run code of one that does (see holderArgRunsCode), nor a
// *gorm.DB method, when no argument is a function. Connection, Transaction,
// FindInBatches, ToSQL and the scopes of Scopes run such a function before
// they return (#345). Any other call, go statement or dynamic call may. So may
// a channel operation, close and a call into sync or sync/atomic, which let a
// goroutine that is already running go on (#345).
func (g *Generator) holderCallChanges(instr ssa.Instruction) bool {
	switch instr := instr.(type) {
	case *ssa.Send, *ssa.Select:
		return true
	case *ssa.UnOp:
		return instr.Op == token.ARROW
	case *ssa.Defer:
		return false
	case ssa.CallInstruction:
		common := instr.Common()
		if b, ok := common.Value.(*ssa.Builtin); ok {
			return b.Name() == "close"
		}
		if site := g.tracer.Site(instr); site.IsGormMethod() {
			return slices.ContainsFunc(site.Args, func(arg ssa.Value) bool { return holderTypeHasFunc(arg.Type()) })
		}
		callee := common.StaticCallee()
		if callee == nil || callee.Pkg == nil || holderSyncPackage(callee.Pkg.Pkg) || g.holderReachesGorm(callee.Pkg.Pkg) {
			return true
		}
		return slices.ContainsFunc(common.Args, g.holderArgRunsCode)
	}
	return false
}

// holderSyncPackage reports whether pkg is sync or sync/atomic, whose calls
// let another goroutine run past a point it waits at.
func holderSyncPackage(pkg *types.Package) bool {
	return pkg.Path() == "sync" || pkg.Path() == "sync/atomic"
}

// holderTypeHasFunc reports whether a value of type t is a function or a
// slice of them, such as a callback or the scopes of Scopes.
func holderTypeHasFunc(t types.Type) bool {
	if slice, ok := t.Underlying().(*types.Slice); ok {
		t = slice.Elem()
	}
	_, ok := t.Underlying().(*types.Signature)
	return ok
}

// holderArgRunsCode reports whether arg, an argument of a call into a package
// that does not reach gorm, lets the callee run code of a package that does
// (#345). sort.Sort calls the methods of its interface argument, and
// json.Marshal those of the value boxed in it. A value boxed in an interface
// is judged by its own type, so fmt.Println(n) runs nothing. So is each value
// of a variadic argument built at the call.
func (g *Generator) holderArgRunsCode(arg ssa.Value) bool {
	switch arg := arg.(type) {
	case *ssa.Const:
		return false
	case *ssa.MakeInterface:
		return g.holderTypeRunsCode(arg.X.Type())
	case *ssa.Slice:
		if array, ok := arg.X.(*ssa.Alloc); ok {
			return slices.ContainsFunc(*array.Referrers(), func(r ssa.Instruction) bool {
				elem, ok := r.(*ssa.IndexAddr)
				if !ok {
					return r != arg
				}
				return slices.ContainsFunc(*elem.Referrers(), func(r ssa.Instruction) bool {
					store, ok := r.(*ssa.Store)
					return !ok || g.holderArgRunsCode(store.Val)
				})
			})
		}
	}
	return g.holderTypeRunsCode(arg.Type())
}

// holderTypeRunsCode reports whether a value of type t can run code of a
// package that reaches gorm when another package uses it: it can hold a
// function, an interface, a type parameter, or a value of a type with methods
// declared in such a package. The result is kept per type.
func (g *Generator) holderTypeRunsCode(t types.Type) bool {
	c := &g.holders
	runs, ok := c.runs[t]
	if !ok {
		if c.runs == nil {
			c.runs = map[types.Type]bool{}
		}
		runs = g.holderTypeRunsCodeIn(t, map[types.Type]bool{})
		c.runs[t] = runs
	}
	return runs
}

// holderTypeRunsCodeIn is holderTypeRunsCode without the memo. seen holds the
// types being searched: a type that holds itself adds nothing.
func (g *Generator) holderTypeRunsCodeIn(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	if named, ok := types.Unalias(t).(*types.Named); ok {
		if pkg := named.Obj().Pkg(); pkg != nil && types.NewMethodSet(types.NewPointer(named)).Len() > 0 && g.holderReachesGorm(pkg) {
			return true
		}
	}
	switch u := t.Underlying().(type) {
	case *types.Signature, *types.Interface, *types.TypeParam:
		return true
	case *types.Pointer:
		return g.holderTypeRunsCodeIn(u.Elem(), seen)
	case *types.Slice:
		return g.holderTypeRunsCodeIn(u.Elem(), seen)
	case *types.Array:
		return g.holderTypeRunsCodeIn(u.Elem(), seen)
	case *types.Chan:
		return g.holderTypeRunsCodeIn(u.Elem(), seen)
	case *types.Map:
		return g.holderTypeRunsCodeIn(u.Key(), seen) || g.holderTypeRunsCodeIn(u.Elem(), seen)
	case *types.Struct:
		for field := range u.Fields() {
			if g.holderTypeRunsCodeIn(field.Type(), seen) {
				return true
			}
		}
	}
	return false
}

// holderReachesGorm memoizes typeutil.ReachesGorm.
func (g *Generator) holderReachesGorm(pkg *types.Package) bool {
	c := &g.holders
	reaches, ok := c.gorm[pkg]
	if !ok {
		if c.gorm == nil {
			c.gorm = map[*types.Package]bool{}
		}
		reaches = typeutil.ReachesGorm(pkg)
		c.gorm[pkg] = reaches
	}
	return reaches
}

// holderNextCall returns the first call in store's block, after it, that
// may change memory other functions reach, and whether a block after that
// block can still run one.
func (g *Generator) holderNextCall(store *ssa.Store) (next ssa.Instruction, later bool) {
	calls := g.holderFuncCalls(store.Parent())
	block := store.Block()
	inBlock := calls.inBlock[block]
	at := calls.index[store]
	if i, _ := slices.BinarySearchFunc(inBlock, at, func(instr ssa.Instruction, at int) int {
		return calls.index[instr] - at
	}); i < len(inBlock) {
		next = inBlock[i]
	}
	return next, slices.ContainsFunc(block.Succs, func(succ *ssa.BasicBlock) bool { return calls.from[succ] })
}
