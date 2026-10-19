package callsite

import (
	"go/constant"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// SessionEffect describes how one call to an immutable-returning builtin
// relates to its receiver and what it returns. Both fields are conservative:
// each is true unless the call is known not to have that effect.
type SessionEffect struct {
	// SharesStatement is true when the result points at the receiver's
	// Statement instead of a copy. The result forks a fresh Statement on each
	// use, so using it never changes the receiver. But when the receiver is
	// clone==0, a later in-place change to the receiver leaks into every later
	// use of the result.
	SharesStatement bool
	// ReturnsMutable is true when the result is a clone==0 handle.
	ReturnsMutable bool
}

// SessionEffect reports the effect of the call, a call to a gorm
// immutable-returning builtin. For Session it reads the *gorm.Session
// argument. The caller must make sure the callee is the real gorm builtin.
//
// GORM v1.31 behaves as follows:
//   - Debug calls Session with only a Logger, so it shares.
//   - WithContext calls Session with a Context, so it does not share.
//   - Session clones the Statement when Context, PrepareStmt or SkipHooks is
//     set. With NewDB, each use starts a new Statement. Otherwise it shares.
//     With Initialized it returns tx.getInstance(): a clone==0 handle that
//     has its own copy of the Statement.
//
// When the argument is not a &gorm.Session{...} literal that this method can
// read, both effects are assumed. A Context field set to anything other
// than the nil constant counts as set, as for WithContext(ctx).
func (s Site) SessionEffect() SessionEffect {
	switch s.Method() {
	case "Debug":
		return SessionEffect{SharesStatement: true}
	case "Session":
		fields, ok := readSessionLiteral(s.sessionConfig())
		if !ok {
			return SessionEffect{SharesStatement: true, ReturnsMutable: true}
		}
		isolated := fields["Context"] == sessionFieldTrue ||
			fields["PrepareStmt"] == sessionFieldTrue ||
			fields["SkipHooks"] == sessionFieldTrue ||
			fields["NewDB"] == sessionFieldTrue
		initialized := fields["Initialized"]
		return SessionEffect{
			SharesStatement: !isolated && initialized != sessionFieldTrue,
			ReturnsMutable:  initialized != sessionFieldFalse,
		}
	default:
		return SessionEffect{}
	}
}

// SessionSetsInitialized reports whether the first argument of the call is a
// &gorm.Session{...} literal that certainly sets Initialized to true. Such a
// Session call returns a clone==0 handle. Unlike SessionEffect, an unknown
// config gives false: this is for checks that report only what they can
// prove.
func (s Site) SessionSetsInitialized() bool {
	fields, ok := readSessionLiteral(s.sessionConfig())
	return ok && fields["Initialized"] == sessionFieldTrue
}

// sessionConfig returns the *gorm.Session argument of a Session call, or nil
// when the call has no argument.
func (s Site) sessionConfig() ssa.Value {
	if len(s.Args) == 0 {
		return nil
	}
	return s.Args[0]
}

// sessionFieldState is what readSessionLiteral knows about one field. The
// zero value is false, which is also the value of a field the literal omits.
type sessionFieldState uint8

const (
	sessionFieldFalse sessionFieldState = iota
	sessionFieldTrue
	sessionFieldUnknown
)

// readSessionLiteral reads the fields of a &gorm.Session{...} value. In SSA,
// such a literal is an Alloc with one FieldAddr and Store per set field. ok is
// false when v is anything else, or when the Alloc is used in some way other
// than a field store or a call to a Session method, since that use could
// change it. A store in another block than the Alloc, such as one under an
// if, makes its field unknown.
func readSessionLiteral(v ssa.Value) (fields map[string]sessionFieldState, ok bool) {
	alloc, isAlloc := v.(*ssa.Alloc)
	if !isAlloc {
		return nil, false
	}
	ptr, isPtr := alloc.Type().Underlying().(*types.Pointer)
	if !isPtr {
		return nil, false
	}
	st, isStruct := ptr.Elem().Underlying().(*types.Struct)
	if !isStruct {
		return nil, false
	}

	fields = map[string]sessionFieldState{}
	for _, ref := range *alloc.Referrers() {
		switch r := ref.(type) {
		case *ssa.FieldAddr:
			name := st.Field(r.Field).Name()
			for _, fieldRef := range *r.Referrers() {
				store, isStore := fieldRef.(*ssa.Store)
				if !isStore || store.Addr != r {
					return nil, false
				}
				state := sessionFieldUnknown
				if store.Block() == alloc.Block() {
					state = sessionStoredState(store.Val)
				}
				if prev, seen := fields[name]; seen && prev != state {
					state = sessionFieldUnknown
				}
				fields[name] = state
			}
		case *ssa.Call:
			if SiteOfWithoutFuncVars(r).Method() != "Session" {
				return nil, false
			}
		case *ssa.DebugRef:
		default:
			return nil, false
		}
	}
	return fields, true
}

// sessionStoredState reads a value stored into a gorm.Session field. A
// constant is read as a bool, where the nil or zero constant is false. Any
// other value is unknown, except for an interface (the Context field), which
// counts as set.
func sessionStoredState(v ssa.Value) sessionFieldState {
	c, isConst := v.(*ssa.Const)
	if !isConst {
		if types.IsInterface(v.Type()) {
			return sessionFieldTrue
		}
		return sessionFieldUnknown
	}
	if c.Value == nil {
		return sessionFieldFalse
	}
	if c.Value.Kind() == constant.Bool && constant.BoolVal(c.Value) {
		return sessionFieldTrue
	}
	return sessionFieldFalse
}
