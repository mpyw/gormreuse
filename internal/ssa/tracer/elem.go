package tracer

import (
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/effort"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// ElemRoot is a *gorm.DB read from a container (#374): an element of a slice
// or an array, a value of a map, a value received from a channel, or what a
// pointer parameter points to.
//
//	m := map[string]*gorm.DB{"adult": base.Where("age > ?", 18)}
//	q := m["adult"]
//	q.Count(&n)
//	q.Find(&us)   // the second branch of what m["adult"] holds
//
// Storing a value into a container is a branch of it already, so a read is not
// traced to what was stored: each read would then be a second branch. A read
// is a root of its own instead, and the reads of one element share it: two
// reads of s[0], or of *p, read one Statement. A receive, or a read of an
// element that the function also writes, is a root of its own.
//
// A read is mutable when the container may hold a mutable value (see
// elemContainerHoldsMutable). The call that may have made the value is not known,
// so no fix appends Session() to it.
type ElemRoot struct {
	// Read is the read of the element that the diagnostics name: the first
	// one in the source.
	Read ssa.Value
}

// Name names the read: "elem t3".
func (r *ElemRoot) Name() string { return "elem " + r.Read.Name() }

// String describes the read.
func (r *ElemRoot) String() string { return "elem " + r.Read.String() }

// Type returns the type of the value read.
func (r *ElemRoot) Type() types.Type { return r.Read.Type() }

// Parent returns the function that reads the element.
func (r *ElemRoot) Parent() *ssa.Function { return r.Read.Parent() }

// Referrers returns nil: no instruction refers to an ElemRoot.
func (r *ElemRoot) Referrers() *[]ssa.Instruction { return nil }

// Pos returns the position of the read, or of the index expression it loads
// through.
func (r *ElemRoot) Pos() token.Pos { return elemReadPos(r.Read) }

// Block returns the block of the read, which defines the value. See RootBlock.
func (r *ElemRoot) Block() *ssa.BasicBlock { return r.Read.(ssa.Instruction).Block() }

// elemReadPos returns the position of read, or of the IndexAddr it loads
// through when the load has none.
func elemReadPos(read ssa.Value) token.Pos {
	if pos := read.Pos(); pos.IsValid() {
		return pos
	}
	if load, ok := read.(*ssa.UnOp); ok {
		return load.X.Pos()
	}
	if e, ok := read.(*ssa.Extract); ok {
		return e.Tuple.Pos()
	}
	return token.NoPos
}

// elemKey is an element that reads share: a container and the key read
// from it. A constant key is kept by its value, since two constants of one
// value are two SSA values. A read that shares with no other has only read
// set.
type elemKey struct {
	container, key ssa.Value
	constKey       string
	read           ssa.Value
}

// elemMemo keeps the root of each read, and whether each container may hold
// a mutable value.
//
//declscope:shared // root.go's RootTracer holds one
type elemMemo struct {
	//declscope:private
	roots map[ssa.Value]ssa.Value
	//declscope:private
	interned map[elemKey]*ElemRoot
	//declscope:private
	mutable map[ssa.Value]bool
	//declscope:private
	accesses map[elemKey]*elemAccess
	//declscope:private
	scanned map[ssa.Value]bool
}

// elemRead returns the container that v reads an element of, and the key of
// the element. keyed is false for a read that no other read shares: a receive
// or a step of a range loop over a map. ok is false when v is no such read.
func elemRead(v ssa.Value) (container, key ssa.Value, keyed, ok bool) {
	if t := v.Type(); !typeutil.IsGormDB(t) && !types.IsInterface(t) {
		return nil, nil, false, false
	}
	switch v := v.(type) {
	case *ssa.UnOp:
		if v.Op == token.ARROW && !v.CommaOk {
			return v.X, nil, false, true
		}
		if v.Op == token.MUL {
			switch x := v.X.(type) {
			case *ssa.IndexAddr:
				return x.X, x.Index, true, true
			case *ssa.Parameter:
				return x, nil, true, true
			}
		}
	case *ssa.Index:
		return v.X, v.Index, true, true
	case *ssa.Lookup:
		if !v.CommaOk {
			return v.X, v.Index, true, true
		}
	case *ssa.Extract:
		switch tuple := v.Tuple.(type) {
		case *ssa.Lookup:
			if v.Index == 0 {
				return tuple.X, tuple.Index, true, true
			}
		case *ssa.UnOp:
			if tuple.Op == token.ARROW && v.Index == 0 {
				return tuple.X, nil, false, true
			}
		case *ssa.Next:
			if r, ok := tuple.Iter.(*ssa.Range); ok && !tuple.IsString && v.Index > 0 {
				return r.X, nil, false, true
			}
		}
	}
	return nil, nil, false, false
}

// elemBuiltinName returns the name of the builtin that call calls, or "".
func elemBuiltinName(call *ssa.CallCommon) string {
	if b, ok := call.Value.(*ssa.Builtin); ok {
		return b.Name()
	}
	return ""
}

// elemRoot returns the root of v when v reads an element of a container (see
// ElemRoot): nil when the container holds no mutable value. ok is false when
// v is no such read.
//
//declscope:shared // root.go's trace and traceAll ask it of the values they reach
func (t *RootTracer) elemRoot(v ssa.Value) (root ssa.Value, ok bool) {
	container, key, keyed, ok := elemRead(v)
	if !ok {
		return nil, false
	}
	m := &t.elems
	if root, done := m.roots[v]; done {
		return root, true
	}
	if m.roots == nil {
		m.roots = map[ssa.Value]ssa.Value{}
		m.interned = map[elemKey]*ElemRoot{}
		m.mutable = map[ssa.Value]bool{}
		m.accesses = map[elemKey]*elemAccess{}
		m.scanned = map[ssa.Value]bool{}
	}
	m.roots[v] = nil
	if !t.elemContainerHoldsMutable(container) {
		return nil, true
	}
	k, read := elemKey{read: v}, v
	if keyed {
		if a := t.elemAccessOf(container, key); !a.written {
			k, read = a.key, firstElemRead(v, a.reads)
		}
	}
	r, found := m.interned[k]
	if !found {
		r = &ElemRoot{Read: read}
		m.interned[k] = r
	}
	m.roots[v] = r
	return r, true
}

// elemKeyOf returns the elemKey of key in container.
func elemKeyOf(container, key ssa.Value) elemKey {
	if c, ok := key.(*ssa.Const); ok {
		return elemKey{container: container, constKey: c.String()}
	}
	return elemKey{container: container, key: key}
}

// elemAccess is the reads of one element in its container's function, and
// the instructions that write it: a store through an index address of the
// container, a map update, or a store through the pointer parameter.
type elemAccess struct {
	key    elemKey
	reads  []ssa.Value
	writes []ssa.Instruction
	// written is set when a write may run after a read, so that two reads
	// may read two values (see elemWrittenBetween).
	written bool
}

// elemAccessOf returns the accesses of the element of container at key. The
// referrers of a container are read once for all its elements.
func (t *RootTracer) elemAccessOf(container, key ssa.Value) *elemAccess {
	m := &t.elems
	k := elemKeyOf(container, key)
	if !m.scanned[container] {
		m.scanned[container] = true
		var found []*elemAccess
		scanElemAccesses(container, func(key ssa.Value, read ssa.Value, write ssa.Instruction) {
			k := elemKey{container: container}
			if key != nil {
				k = elemKeyOf(container, key)
			}
			a := m.accesses[k]
			if a == nil {
				a = &elemAccess{key: k}
				m.accesses[k] = a
				found = append(found, a)
			}
			if read != nil {
				a.reads = append(a.reads, read)
			}
			if write != nil {
				a.writes = append(a.writes, write)
			}
		})
		for _, a := range found {
			a.written = t.elemWrittenBetween(a.reads, a.writes)
		}
	}
	if a := m.accesses[k]; a != nil {
		return a
	}
	return &elemAccess{key: k}
}

// scanElemAccesses calls visit with each read and write of an element of
// container, and the key of the element: nil for what a pointer points to.
func scanElemAccesses(container ssa.Value, visit func(key, read ssa.Value, write ssa.Instruction)) {
	refs := container.Referrers()
	if refs == nil {
		return
	}
	for _, ref := range *refs {
		switch ref := ref.(type) {
		case *ssa.IndexAddr:
			for _, use := range *ref.Referrers() {
				switch use := use.(type) {
				case *ssa.UnOp:
					if use.Op == token.MUL {
						visit(ref.Index, use, nil)
					}
				case *ssa.Store:
					if use.Addr == ssa.Value(ref) {
						visit(ref.Index, nil, use)
					}
				}
			}
		case *ssa.UnOp:
			if ref.Op == token.MUL {
				visit(nil, ref, nil)
			}
		case *ssa.Store:
			if ref.Addr == container {
				visit(nil, nil, ref)
			}
		case *ssa.Index:
			visit(ref.Index, ref, nil)
		case *ssa.Lookup:
			if !ref.CommaOk {
				visit(ref.Index, ref, nil)
				continue
			}
			for _, use := range *ref.Referrers() {
				if e, ok := use.(*ssa.Extract); ok && e.Index == 0 {
					visit(ref.Index, e, nil)
				}
			}
		case *ssa.MapUpdate:
			visit(ref.Key, nil, ref)
		}
	}
}

// elemWrittenBetween reports whether one of writes may run after one of reads,
// so that two reads may read two values. A write before every read, such as
// the one that fills a composite literal, does not part them.
func (t *RootTracer) elemWrittenBetween(reads []ssa.Value, writes []ssa.Instruction) bool {
	for _, w := range writes {
		for _, read := range reads {
			effort.Step()
			if t.elemRunsAfter(read.(ssa.Instruction), w) {
				return true
			}
		}
	}
	return false
}

// elemRunsAfter reports whether b may run after a, both in one function.
func (t *RootTracer) elemRunsAfter(a, b ssa.Instruction) bool {
	ab, bb := a.Block(), b.Block()
	if ab != bb {
		return t.cfgs.CanReach(ab, bb)
	}
	if slices.Index(ab.Instrs, b) > slices.Index(ab.Instrs, a) {
		return true
	}
	return slices.ContainsFunc(ab.Succs, func(s *ssa.BasicBlock) bool { return t.cfgs.CanReach(s, ab) })
}

// firstElemRead returns the read among reads that comes first in the source,
// so the root does not depend on which read is traced first. It is v when no
// read has a position.
func firstElemRead(v ssa.Value, reads []ssa.Value) ssa.Value {
	first := v
	for _, read := range reads {
		if p := elemReadPos(read); p.IsValid() && (!elemReadPos(first).IsValid() || p < elemReadPos(first)) {
			first = read
		}
	}
	return first
}

// elemContainerHoldsMutable reports whether container may hold a value with a
// mutable root. It collects the values that may be the same container: those
// it is made from, a slice of it, a merge, the result of append, a variable
// that holds it, a closure's capture of it. The container may hold a mutable
// value when
//
//   - one of them is a parameter or the result of a call, whose elements
//     come from code not seen here;
//   - one of them is passed to a function that may reach gorm, which may store
//     into it; or
//   - a value stored into one of them, sent on it or appended to it has a
//     mutable root.
//
// A container read from a field or a package-level variable is not followed,
// as a field read through a parameter is not (see Known Limitations). A
// container whose answer is being found holds nothing more than what the
// others give.
func (t *RootTracer) elemContainerHoldsMutable(container ssa.Value) bool {
	m := &t.elems
	if mutable, ok := m.mutable[container]; ok {
		return mutable
	}
	members, order, unknown := elemContainerAliases(container, t.FuncVars())
	for _, v := range order {
		m.mutable[v] = unknown
	}
	if unknown {
		return true
	}
	mutable := false
	for _, v := range order {
		for _, elem := range containerElems(v, members) {
			if t.FindMutableRoot(elem, nil) != nil {
				mutable = true
				break
			}
		}
		if mutable {
			break
		}
	}
	for _, v := range order {
		m.mutable[v] = mutable
	}
	return mutable
}

// elemContainerAliases returns the values that may be container itself, as
// elemContainerHoldsMutable lists them, as a set and in the order found. unknown
// is set when one of them comes from code not seen here, or is handed to such
// code.
func elemContainerAliases(container ssa.Value, vars callsite.FuncVars) (members map[ssa.Value]bool, order []ssa.Value, unknown bool) {
	members = map[ssa.Value]bool{}
	work := []ssa.Value{container}
	add := func(v ssa.Value) {
		if v != nil && !members[v] {
			work = append(work, v)
		}
	}
	for len(work) > 0 {
		v := work[len(work)-1]
		work = work[:len(work)-1]
		if members[v] {
			continue
		}
		members[v] = true
		order = append(order, v)
		effort.Step()
		if elemAliasSources(v, vars, add) {
			unknown = true
		}
		if elemAliasUses(v, vars, add) {
			unknown = true
		}
	}
	return members, order, unknown
}

// elemAliasSources adds what v, a container, is made from. It reports whether
// v comes from code not seen here: a parameter or a call.
func elemAliasSources(v ssa.Value, vars callsite.FuncVars, add func(ssa.Value)) (unknown bool) {
	switch v := v.(type) {
	case *ssa.Parameter:
		return true
	case *ssa.Call:
		if callsite.SiteOf(v, vars).Kind != callsite.SiteBuiltin {
			return true
		}
		if elemBuiltinName(&v.Call) == "append" {
			for _, arg := range v.Call.Args {
				add(arg)
			}
		}
	case *ssa.Extract:
		_, call := v.Tuple.(*ssa.Call)
		return call
	case *ssa.Phi:
		for _, edge := range v.Edges {
			add(edge)
		}
	case *ssa.Slice:
		add(v.X)
	case *ssa.ChangeType:
		add(v.X)
	case *ssa.Convert:
		add(v.X)
	case *ssa.MakeInterface:
		add(v.X)
	case *ssa.TypeAssert:
		add(v.X)
	case *ssa.UnOp:
		if v.Op == token.MUL {
			switch v.X.(type) {
			case *ssa.Alloc, *ssa.FreeVar:
				add(v.X)
			}
		}
	case *ssa.FreeVar:
		add(callsite.FreeVarBinding(v))
	}
	return false
}

// elemAliasUses adds where v, a container, goes: a variable that holds it, a
// load of such a variable, a slice or a conversion of it, a merge, a closure's
// capture, append and copy. It reports whether v is handed to code that may
// store into it.
func elemAliasUses(v ssa.Value, vars callsite.FuncVars, add func(ssa.Value)) (unknown bool) {
	refs := v.Referrers()
	if refs == nil {
		return false
	}
	for _, ref := range *refs {
		switch ref := ref.(type) {
		case *ssa.Store:
			if ref.Addr == v {
				add(ref.Val) // a variable that holds the container
			} else if _, local := ref.Addr.(*ssa.Alloc); local && ref.Val == v {
				add(ref.Addr)
			}
		case *ssa.UnOp:
			if ref.Op == token.MUL {
				add(ref)
			}
		case *ssa.Slice, *ssa.ChangeType, *ssa.Convert, *ssa.Phi:
			add(ref.(ssa.Value))
		case *ssa.MakeClosure:
			fn, ok := ref.Fn.(*ssa.Function)
			for i, b := range ref.Bindings {
				if ok && b == v && i < len(fn.FreeVars) {
					add(fn.FreeVars[i])
				}
			}
		case ssa.CallInstruction:
			if elemAliasCall(ref, vars, add) {
				unknown = true
			}
		}
	}
	return unknown
}

// elemAliasCall adds what call shares a container's elements with: the
// arguments of append and copy, and append's result. It reports whether the
// call may store into the container: any call but a builtin and a function
// that cannot name gorm, which makes no *gorm.DB.
func elemAliasCall(call ssa.CallInstruction, vars callsite.FuncVars, add func(ssa.Value)) (unknown bool) {
	site := callsite.SiteOf(call, vars)
	switch {
	case site.Kind == callsite.SiteBuiltin:
		if name := elemBuiltinName(call.Common()); name == "append" || name == "copy" {
			for _, arg := range call.Common().Args {
				add(arg)
			}
			if c, ok := call.(*ssa.Call); ok && name == "append" {
				add(c)
			}
		}
		return false
	case site.Declared != nil && site.Declared.Object() != nil:
		return typeutil.ReachesGorm(site.Declared.Object().Pkg())
	}
	return true
}

// containerElems returns the values stored into v, a container of members,
// as elements: through an index address of it, a map update, a send, or the
// elements of append. A value stored into v itself, such as a slice stored
// into a variable, is a member already.
func containerElems(v ssa.Value, members map[ssa.Value]bool) []ssa.Value {
	refs := v.Referrers()
	if refs == nil {
		return nil // a constant, such as a nil slice
	}
	var elems []ssa.Value
	for _, ref := range *refs {
		switch ref := ref.(type) {
		case *ssa.IndexAddr:
			for _, w := range *ref.Referrers() {
				if s, ok := w.(*ssa.Store); ok && s.Addr == ssa.Value(ref) && !members[s.Val] {
					elems = append(elems, s.Val)
				}
			}
		case *ssa.MapUpdate:
			if ref.Map == v {
				elems = append(elems, ref.Value)
			}
		case *ssa.Send:
			if ref.Chan == v {
				elems = append(elems, ref.X)
			}
		case *ssa.Store:
			if ref.Addr == v && !members[ref.Val] {
				elems = append(elems, ref.Val) // through a pointer to an element
			}
		}
	}
	return elems
}
