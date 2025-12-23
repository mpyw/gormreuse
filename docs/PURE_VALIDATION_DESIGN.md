# Pure Function Validation Design

This document describes the design for validating `//gormreuse:pure` directive using a 3-state purity model.

## Overview

### Problem Statement

The current 2-state model (Immutable/Mutable) cannot accurately express the semantics of pure functions that return their arguments unchanged:

```go
//gormreuse:pure
func identity(db *gorm.DB) *gorm.DB {
    return db  // Is this OK? Current model says NO (returns mutable)
}
```

### Solution: 3-State Purity Model

Introduce a third state `Depends` to express values whose purity depends on their source:

| State | Meaning | Example |
|-------|---------|---------|
| `Clean` | Always immutable, safe to reuse | `db.Session(&gorm.Session{})` |
| `Polluted` | Tainted, unsafe to reuse | `db.Where("x")` result after use |
| `Depends(param)` | Purity depends on the referenced parameter | `return db` (same as input) |

## Purity State Semantics

### State Transitions

```
             ┌─────────────────────────────────────────┐
             │                                         │
             ▼                                         │
    ┌─────────────┐    Session/WithContext    ┌───────┴─────┐
    │   Clean     │◄──────────────────────────│   Depends   │
    │ (Immutable) │                           │  (on param) │
    └─────────────┘                           └─────────────┘
             │                                         │
             │ Where/Find/etc.                         │ Where/Find/etc.
             │ (chain method)                          │ (chain method)
             ▼                                         ▼
    ┌─────────────┐                           ┌─────────────┐
    │  Polluted   │                           │  Polluted   │
    │  (Tainted)  │                           │  (Tainted)  │
    └─────────────┘                           └─────────────┘
```

### State Analysis Rules

```go
func analyzeState(v ssa.Value) PurityState {
    switch v := v.(type) {
    case *ssa.Parameter:
        return Depends(v)  // Depends on this parameter

    case *ssa.Const:
        return Clean  // nil is always clean

    case *ssa.Call:
        if isPureBuiltinMethod(callee) {
            return Clean  // Session, WithContext, Begin, etc.
        }
        if isPureUserDefinedFunction(callee) {
            // Analyze callee's return state
            return analyzeReturnState(callee)
        }
        return Polluted  // Non-pure call

    case *ssa.Phi:
        return mergeStates(edges...)  // Lattice merge
    }
}
```

### State Lattice for Phi Nodes

When merging states at Phi nodes:

```
Clean ⊔ Clean       = Clean
Clean ⊔ Depends(p)  = Depends(p)
Depends(p) ⊔ Depends(p) = Depends(p)
Depends(p) ⊔ Depends(q) = Depends({p,q})  // Multiple dependencies
* ⊔ Polluted        = Polluted
```

## Pure Function Contract

A function marked `//gormreuse:pure` must satisfy:

1. **No Argument Pollution**: Does not call non-pure methods on `*gorm.DB` arguments
2. **Valid Return State**: Return value must be `Clean` or `Depends` (not `Polluted`)

### Valid Pure Functions

```go
//gormreuse:pure
func identity(db *gorm.DB) *gorm.DB {
    return db  // OK: Depends(db)
}

//gormreuse:pure
func wrap(db *gorm.DB) *gorm.DB {
    return db.Session(&gorm.Session{})  // OK: Clean
}

//gormreuse:pure
func conditional(db *gorm.DB, cond bool) *gorm.DB {
    if cond {
        return db.Session(&gorm.Session{})  // Clean
    }
    return db  // Depends(db)
}  // OK: Depends(db) (conservative merge)
```

### Invalid Pure Functions

```go
//gormreuse:pure
func polluter(db *gorm.DB) *gorm.DB {
    return db.Where("x")  // NG: Polluted (calls non-pure method)
}

//gormreuse:pure
func sneakyPolluter(db *gorm.DB) *gorm.DB {
    db.Find(nil)  // NG: Pollutes argument
    return db.Session(&gorm.Session{})
}
```

## Caller-Side Analysis

When calling a pure function, the return value's state affects tracking:

```go
q := db.Where("x")        // q: mutable root R1
r := pureIdentity(q)      // pureIdentity returns Depends(param)
                          // → r shares root R1 with q
s := pureWrap(q)          // pureWrap returns Clean
                          // → s is a new immutable root

q.Find(nil)               // R1 becomes polluted
r.Find(nil)               // NG: r shares R1, which is polluted
s.Find(nil)               // OK: s is independent
```

## Package Structure

```
internal/
├── purity/                    # New package for purity analysis
│   ├── state.go              # PurityState type and operations
│   ├── analyzer.go           # SSA-based state analyzer
│   ├── validator.go          # Pure function contract validator
│   └── state_test.go         # Unit tests
├── analyzer.go               # Main analyzer (uses purity package)
├── root_tracer.go            # Updated to use PurityState
├── pollution_tracker.go      # Updated to handle Depends state
└── ...
```

## Type Definitions

### `internal/purity/state.go`

```go
package purity

import "golang.org/x/tools/go/ssa"

// StateKind represents the kind of purity state.
type StateKind int

const (
    KindClean StateKind = iota
    KindPolluted
    KindDepends
)

// State represents the purity state of a *gorm.DB value.
type State struct {
    Kind    StateKind
    Deps    []*ssa.Parameter  // Non-nil only for KindDepends
}

// Clean returns a Clean state.
func Clean() State {
    return State{Kind: KindClean}
}

// Polluted returns a Polluted state.
func Polluted() State {
    return State{Kind: KindPolluted}
}

// Depends returns a Depends state with the given parameters.
func Depends(params ...*ssa.Parameter) State {
    return State{Kind: KindDepends, Deps: params}
}

// Merge merges two states using lattice rules.
func (s State) Merge(other State) State {
    // Implementation of lattice merge
}

// IsClean returns true if the state is Clean.
func (s State) IsClean() bool {
    return s.Kind == KindClean
}

// IsPolluted returns true if the state is Polluted.
func (s State) IsPolluted() bool {
    return s.Kind == KindPolluted
}

// IsDepends returns true if the state is Depends.
func (s State) IsDepends() bool {
    return s.Kind == KindDepends
}
```

### `internal/purity/analyzer.go`

```go
package purity

import "golang.org/x/tools/go/ssa"

// Analyzer analyzes purity states of SSA values.
type Analyzer struct {
    fn         *ssa.Function
    gormParams map[*ssa.Parameter]bool
    cache      map[ssa.Value]State
}

// NewAnalyzer creates a new purity analyzer for the given function.
func NewAnalyzer(fn *ssa.Function) *Analyzer

// AnalyzeValue returns the purity state of the given SSA value.
func (a *Analyzer) AnalyzeValue(v ssa.Value) State

// AnalyzeReturn returns the purity state of the function's return value.
func (a *Analyzer) AnalyzeReturn() State
```

### `internal/purity/validator.go`

```go
package purity

import (
    "go/token"
    "golang.org/x/tools/go/ssa"
)

// Violation represents a pure function contract violation.
type Violation struct {
    Pos     token.Pos
    Message string
}

// ValidateFunction validates that a function satisfies the pure contract.
func ValidateFunction(fn *ssa.Function, pureFuncs PureFuncChecker) []Violation
```

## Implementation Plan

### Phase 1: Core Types (purity/state.go)
- [ ] Define `StateKind` enum
- [ ] Define `State` struct with `Kind` and `Deps`
- [ ] Implement `Clean()`, `Polluted()`, `Depends()` constructors
- [ ] Implement `Merge()` with lattice rules
- [ ] Implement helper methods (`IsClean()`, etc.)
- [ ] Write unit tests

### Phase 2: State Analyzer (purity/analyzer.go)
- [ ] Implement `Analyzer` struct
- [ ] Implement `AnalyzeValue()` for each SSA value type:
  - [ ] `*ssa.Parameter` → Depends
  - [ ] `*ssa.Const` → Clean
  - [ ] `*ssa.Call` → Clean/Polluted based on callee
  - [ ] `*ssa.Phi` → Merge all edges
  - [ ] `*ssa.Extract` → Trace to tuple
  - [ ] `*ssa.UnOp` (dereference) → Trace through
- [ ] Handle cycles with visited map
- [ ] Write unit tests

### Phase 3: Validator (purity/validator.go)
- [ ] Implement `ValidateFunction()`
- [ ] Check for argument pollution (non-pure method calls)
- [ ] Check return state (must be Clean or Depends)
- [ ] Generate violation messages
- [ ] Write unit tests

### Phase 4: Integration
- [ ] Update `internal/analyzer.go` to use purity package
- [ ] Update `RootTracer` to handle Depends state
- [ ] Update `PollutionTracker` to track shared roots
- [ ] Update testdata with new test cases
- [ ] Run full test suite

### Phase 5: Cleanup
- [ ] Remove old `pure_validator.go`
- [ ] Update CLAUDE.md with new architecture
- [ ] Document public APIs

## Testing Strategy

### Unit Tests (purity package)
- State operations (Merge, constructors)
- Analyzer for each SSA value type
- Validator for valid/invalid pure functions

### Integration Tests (testdata)
- Pure functions returning Clean
- Pure functions returning Depends
- Invalid pure functions (Polluted return)
- Caller-side tracking with Depends

## Migration Notes

The current `internal/pure_validator.go` will be replaced by the new `internal/purity/` package. The new implementation provides:

1. More accurate modeling with 3 states
2. Better separation of concerns (state analysis vs validation)
3. Cleaner namespace (all purity-related types in one package)
4. Easier testing (smaller, focused units)
