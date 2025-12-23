// Package purity provides 3-state purity analysis for *gorm.DB values.
//
// The purity model uses three states:
//   - Clean: Always immutable, safe to reuse (e.g., Session() result)
//   - Polluted: Tainted by chain methods, unsafe to reuse
//   - Depends: Purity depends on referenced parameters
package purity

import "golang.org/x/tools/go/ssa"

// =============================================================================
// State Kind
// =============================================================================

// Kind represents the kind of purity state.
type Kind int

const (
	// KindClean represents a value that is always immutable.
	KindClean Kind = iota
	// KindPolluted represents a value that is tainted and unsafe to reuse.
	KindPolluted
	// KindDepends represents a value whose purity depends on parameters.
	KindDepends
)

// =============================================================================
// Purity State
// =============================================================================

// State represents the purity state of a *gorm.DB value.
type State struct {
	kind Kind
	deps []*ssa.Parameter // Non-nil only for KindDepends
}

// =============================================================================
// State Constructors
// =============================================================================

// Clean returns a new Clean state.
func Clean() State {
	return State{kind: KindClean}
}

// Polluted returns a new Polluted state.
func Polluted() State {
	return State{kind: KindPolluted}
}

// Depends returns a new Depends state with the given parameters.
// Parameters are deduplicated. Order is not guaranteed.
func Depends(params ...*ssa.Parameter) State {
	if len(params) == 0 {
		return Clean()
	}

	seen := make(map[*ssa.Parameter]bool)
	unique := make([]*ssa.Parameter, 0, len(params))
	for _, p := range params {
		if p != nil && !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}

	if len(unique) == 0 {
		return Clean()
	}

	return State{kind: KindDepends, deps: unique}
}

// =============================================================================
// State Accessors
// =============================================================================

// Deps returns the parameter dependencies for Depends states.
func (s State) Deps() []*ssa.Parameter {
	return s.deps
}

// IsPolluted returns true if the state is Polluted.
func (s State) IsPolluted() bool {
	return s.kind == KindPolluted
}

// IsDepends returns true if the state is Depends.
func (s State) IsDepends() bool {
	return s.kind == KindDepends
}

// =============================================================================
// State Operations
// =============================================================================

// Merge merges two states using lattice rules.
//
// Lattice order: Clean < Depends < Polluted
//
// Merge rules:
//   - Clean ⊔ Clean = Clean
//   - Clean ⊔ Depends(p) = Depends(p)
//   - Depends(p) ⊔ Depends(q) = Depends(p, q)
//   - * ⊔ Polluted = Polluted
func (s State) Merge(other State) State {
	if s.kind == KindPolluted || other.kind == KindPolluted {
		return Polluted()
	}

	if s.kind == KindClean && other.kind == KindClean {
		return Clean()
	}

	var allDeps []*ssa.Parameter
	if s.kind == KindDepends {
		allDeps = append(allDeps, s.deps...)
	}
	if other.kind == KindDepends {
		allDeps = append(allDeps, other.deps...)
	}

	return Depends(allDeps...)
}
