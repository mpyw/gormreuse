package callback

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/callsite"
)

// holders are the package-level variables and struct fields that hold one
// function, so that a callback read from them is the function stored there
// (#257):
//
//	var txBody = func(tx *gorm.DB) error { ... }
//	db.Transaction(txBody)
//
//	s := &svc{cb: func(tx *gorm.DB) error { ... }}
//	db.Transaction(s.cb)
//
// A variable is one of callsite.FuncVars. A field is held when it is
// unexported, the package stores to it exactly once, and its address is used
// only to load and store it. The single store may run many times, but every
// value it stores is the same function, or a closure of it. An exported field
// may be assigned by another package, and a second store or a pointer to the
// field may put another function there.
//
//declscope:shared // collect.go and reach.go build it, resolve.go and reach.go read through it
type holders struct {
	// vars are the held variables. reach.go reads calls through them.
	vars callsite.FuncVars
	// fields maps each held field to the value its single store stores.
	//
	//declscope:private
	fields map[*types.Var]ssa.Value
	// varLoads and fieldLoads are every read of a held variable or field: a
	// load of the variable, a load of the field's address, or the field of a
	// struct held in a register.
	//
	//declscope:private
	varLoads map[*ssa.Global][]ssa.Value
	//declscope:private
	fieldLoads map[*types.Var][]ssa.Value
	// checking stops a cycle of holders that feed each other. It is keyed
	// by the variable or the field.
	//
	//declscope:private
	checking map[any]bool
}

// collectHolders finds the holders of the package that funcs belong to. funcs
// are the analyzed functions; the package initializers are added, since they
// run package-level `var x = ...` initializers.
//
//declscope:shared // collect.go and reach.go build the holders with it
func collectHolders(funcs []*ssa.Function, vars callsite.FuncVars) *holders {
	h := &holders{
		vars:       vars,
		fields:     make(map[*types.Var]ssa.Value),
		varLoads:   make(map[*ssa.Global][]ssa.Value),
		fieldLoads: make(map[*types.Var][]ssa.Value),
		checking:   make(map[any]bool),
	}
	stores := make(map[*types.Var][]ssa.Value)
	escaped := make(map[*types.Var]bool)
	callsite.WalkInstrs(withHolderInits(funcs), func(instr ssa.Instruction) {
		switch x := instr.(type) {
		case *ssa.FieldAddr:
			field := holderFieldOfAddr(x)
			for _, ref := range *x.Referrers() {
				switch r := ref.(type) {
				case *ssa.Store:
					if r.Addr == x {
						stores[field] = append(stores[field], r.Val)
						continue
					}
				case *ssa.UnOp:
					if r.Op == token.MUL {
						h.fieldLoads[field] = append(h.fieldLoads[field], r)
						continue
					}
				case *ssa.DebugRef:
					continue
				}
				escaped[field] = true
			}
		case *ssa.Field:
			field := holderFieldOf(x)
			h.fieldLoads[field] = append(h.fieldLoads[field], x)
		case *ssa.UnOp:
			if g, ok := x.X.(*ssa.Global); ok && x.Op == token.MUL && vars[g] != nil {
				h.varLoads[g] = append(h.varLoads[g], x)
			}
		case *ssa.ChangeType:
			// A struct converted to another struct type with the same
			// fields is read through the other type's fields.
			escapeHolderFields(x.X.Type(), escaped)
		case *ssa.Convert:
			escapeHolderFields(x.X.Type(), escaped)
		}
	})
	for field, vals := range stores {
		if len(vals) == 1 && !escaped[field] && !field.Exported() {
			h.fields[field] = vals[0]
		}
	}
	return h
}

// withHolderInits returns funcs with the initializer of each of their
// packages.
//
//declscope:shared // reach.go scans the same functions
func withHolderInits(funcs []*ssa.Function) []*ssa.Function {
	scan := append([]*ssa.Function(nil), funcs...)
	seen := make(map[*ssa.Package]bool)
	for _, fn := range funcs {
		if fn.Pkg != nil && !seen[fn.Pkg] {
			seen[fn.Pkg] = true
			if init := fn.Pkg.Func("init"); init != nil {
				scan = append(scan, init)
			}
		}
	}
	return scan
}

// holderFieldOfAddr returns the field that fa takes the address of.
func holderFieldOfAddr(fa *ssa.FieldAddr) *types.Var {
	st := fa.X.Type().Underlying().(*types.Pointer).Elem().Underlying().(*types.Struct)
	return st.Field(fa.Field).Origin()
}

// holderFieldOf returns the field that f reads.
func holderFieldOf(f *ssa.Field) *types.Var {
	return f.X.Type().Underlying().(*types.Struct).Field(f.Field).Origin()
}

// escapeHolderFields marks every field of t, a struct or a pointer to
// one, as escaped.
func escapeHolderFields(t types.Type, escaped map[*types.Var]bool) {
	if ptr, ok := t.Underlying().(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if st, ok := t.Underlying().(*types.Struct); ok {
		for field := range st.Fields() {
			escaped[field.Origin()] = true
		}
	}
}

// heldValue returns the value that v reads from a holder: the function a
// held variable holds, or the value stored into a held field. It is nil when
// v is not a read of a holder.
//
//declscope:shared // resolve.go's exact query reads through it
func (h *holders) heldValue(v ssa.Value) ssa.Value {
	if h == nil {
		return nil
	}
	switch x := v.(type) {
	case *ssa.UnOp:
		if x.Op != token.MUL {
			return nil
		}
		switch addr := x.X.(type) {
		case *ssa.Global:
			if fn := h.vars[addr]; fn != nil {
				return fn
			}
		case *ssa.FieldAddr:
			return h.fields[holderFieldOfAddr(addr)]
		}
	case *ssa.Field:
		return h.fields[holderFieldOf(x)]
	}
	return nil
}

// feedsOnlyCallbacks reports whether addr is a holder whose every read hands
// the value to an immutable callback position (see refReachesOnlyCallback).
// A store of a callback into such a holder reaches only those positions.
//
//declscope:shared // reach.go accepts a store into a holder with it
func (h *holders) feedsOnlyCallbacks(addr ssa.Value, inputs *directive.ImmutableInputSet) bool {
	var key any
	var loads []ssa.Value
	switch a := addr.(type) {
	case *ssa.Global:
		if h.vars[a] == nil {
			return false
		}
		key, loads = a, h.varLoads[a]
	case *ssa.FieldAddr:
		field := holderFieldOfAddr(a)
		if _, ok := h.fields[field]; !ok {
			return false
		}
		key, loads = field, h.fieldLoads[field]
	default:
		return false
	}
	if h.checking[key] {
		return false
	}
	h.checking[key] = true
	defer delete(h.checking, key)
	for _, load := range loads {
		for _, ref := range *load.Referrers() {
			if !refReachesOnlyCallback(ref, load, inputs, h) {
				return false
			}
		}
	}
	return true
}
