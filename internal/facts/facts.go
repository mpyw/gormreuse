// Package facts carries what gormreuse found about a package's functions to
// the packages that import it (#195).
//
// The analysis of a package exports a Func fact for each of its exported
// functions and methods that carries a directive. Only those can be called by
// name from another package. A package that calls such a function reads the
// fact instead of the callee's source, so a directive placed anywhere, on a
// generic function, or in a package whose source the driver does not load
// works the same as in the declaring package, and a contract that the callee's
// body breaks is not trusted.
package facts

import (
	"fmt"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
)

// Func is the fact exported for a function or method. The zero value means
// the function has no directive, which is also what a function without a fact
// gets.
type Func struct {
	// Pure is set when the function is marked //gormreuse:pure.
	Pure bool
	// PureFailed is set when the body of a pure function definitively breaks
	// the contract. Such a function is not trusted as pure, and a pure
	// function that passes it its argument breaks the contract too (#187).
	PureFailed bool
	// Returned holds the indexes into the parameters, the receiver first, that
	// the result of a trusted pure function may be (#188).
	Returned []int
	// ReturnsOnlyParams is set when every mutable root the function returns is
	// one of its parameters, so it returns an immutable value when its
	// arguments are immutable. It is only set when Returned is not empty.
	ReturnsOnlyParams bool
	// ImmutableReturn is set when the function is marked
	// //gormreuse:immutable-return and its body keeps the contract.
	ImmutableReturn bool
	// BranchesImmutableParam is set when the function is marked
	// //gormreuse:immutable-param and branches a *gorm.DB parameter, so a
	// caller must pass it an isolated value.
	BranchesImmutableParam bool
	// ImmutableInput holds the //gormreuse:immutable-input(name) callbacks of
	// a function whose body keeps the contract.
	ImmutableInput []directive.ImmutableInputCallback
}

// AFact marks Func as an analysis fact.
func (*Func) AFact() {}

// String lists the fields that are set, for fact expectations in tests.
func (f *Func) String() string {
	words := []string{"gormreuse"}
	add := func(set bool, word string) {
		if set {
			words = append(words, word)
		}
	}
	add(f.Pure, "pure")
	add(f.PureFailed, "pure-failed")
	add(len(f.Returned) > 0, fmt.Sprintf("returns%v", f.Returned))
	add(f.ReturnsOnlyParams, "returns-only-params")
	add(f.ImmutableReturn, "immutable-return")
	add(f.BranchesImmutableParam, "immutable-param")
	for _, cb := range f.ImmutableInput {
		add(true, "immutable-input("+cb.ParamName+")")
	}
	return strings.Join(words, " ")
}

// isZero reports whether f says nothing.
func (f *Func) isZero() bool {
	return !f.Pure && !f.PureFailed && len(f.Returned) == 0 && !f.ReturnsOnlyParams &&
		!f.ImmutableReturn && !f.BranchesImmutableParam && len(f.ImmutableInput) == 0
}

// Table reads the facts of functions declared in other packages. A nil Table
// knows nothing.
type Table struct {
	pass  *analysis.Pass
	cache map[*types.Func]Func
}

// NewTable returns a table that imports facts through pass.
func NewTable(pass *analysis.Pass) *Table {
	return &Table{pass: pass, cache: make(map[*types.Func]Func)}
}

// Of returns the fact of fn, or the zero Func when fn is declared in the
// package being analyzed, has no fact, or is not a declared function. fn may be
// a generic instance or a bound method wrapper (see directive.DeclaredFunc).
func (t *Table) Of(fn *ssa.Function) Func {
	if t == nil || fn == nil {
		return Func{}
	}
	obj, ok := directive.DeclaredFunc(fn).Object().(*types.Func)
	if !ok {
		return Func{}
	}
	obj = obj.Origin()
	if obj.Pkg() == nil || obj.Pkg() == t.pass.Pkg {
		return Func{}
	}
	if f, ok := t.cache[obj]; ok {
		return f
	}
	var f Func
	t.pass.ImportObjectFact(obj, &f)
	t.cache[obj] = f
	return f
}

// Export exports f for fn, a function of the package being analyzed. Nothing
// is exported for a zero f, or for a function that another package cannot
// name: a closure, or an unexported function or method.
func Export(pass *analysis.Pass, fn *ssa.Function, f Func) {
	obj, ok := fn.Object().(*types.Func)
	if !ok || !obj.Exported() || obj.Pkg() != pass.Pkg || f.isZero() {
		return
	}
	pass.ExportObjectFact(obj, &f)
}
