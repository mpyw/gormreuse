# Code Review - Tech Lead Review

Date: 2025-12-23
Reviewer: Claude (Tech Lead perspective)
Status: **34 issues identified**

---

## Summary

| Severity | Count |
|----------|-------|
| Critical (Design Issues) | 5 |
| Major (Code Quality) | 15 |
| Minor (Style/Cleanup) | 14 |

---

## Critical Issues (Design Flaws)

### C1. Empty Structs Everywhere (Anti-pattern)
**Files**: `tracer.go`, `pollution.go`, `handler.go`

```go
type SSATracer struct{}
type CFGAnalyzer struct{}
type CallHandler struct{}
type GoHandler struct{}
// ... 6+ empty structs
```

**Problem**: Stateless structs that should be functions. Using structs adds unnecessary indirection without benefit.

**Recommendation**: Convert to package-level functions or use a single analyzer struct that holds configuration.

---

### C2. Strategy Pattern Overhead Without Benefit
**File**: `handler.go`, `analyzer.go`

```go
for _, handler := range a.handlers {
    if handler.CanHandle(instr) {
        handler.Handle(instr, ctx)
        break
    }
}
```

**Problem**: O(n) dispatch for every instruction. With 6 handlers, this means 6 type checks per instruction.

**Recommendation**: Use type switch directly:
```go
switch instr := instr.(type) {
case *ssa.Call:
    handleCall(instr, ctx)
case *ssa.Go:
    handleGo(instr, ctx)
// ...
}
```

---

### C3. exports.go - Layering Violation Cover-up
**File**: `internal/exports.go`

**Problem**: 77 lines of pure re-exports. This file exists to paper over a package structure problem.

```go
type IgnoreMap = directive.IgnoreMap
type PureFuncSet = directive.PureFuncSet
// ... 14 type aliases
```

**Recommendation**: Either:
1. Expose `directive` and `ssa` packages directly
2. Merge into a single package
3. Accept the deeper import paths

---

### C4. RunSSA Has 6 Parameters (God Function Smell)
**File**: `analyzer.go:21`

```go
func RunSSA(
    pass *analysis.Pass,
    ssaInfo *buildssa.SSA,
    ignoreMaps map[string]directive.IgnoreMap,
    funcIgnores map[string]map[token.Pos]struct{},
    pureFuncs *directive.PureFuncSet,
    skipFiles map[string]bool,
)
```

**Problem**: Too many parameters indicate the function is doing too much or needs a context struct.

**Recommendation**: Create `AnalysisContext` struct.

---

### C5. Adapter Hell
**File**: `purity_adapter.go`

```go
func (c *purityChecker) IsGormDB(t types.Type) bool {
    return typeutil.IsGormDB(t)  // Just a proxy
}
```

**Problem**: Entire file is just proxying calls to `typeutil`. Multiple layers of indirection.

---

## Major Issues (Code Quality)

### M1. Massive Code Duplication in Handlers
**File**: `handler.go`

`CallHandler.Handle` and `processBoundMethodCall` have identical pollution-checking logic (15+ lines duplicated).

`GoHandler.processCallCommon` and `DeferHandler.processCallCommon` are nearly identical (40 lines each).

**Fix**: Extract common logic to shared helper functions.

---

### M2. IsPureFunctionDecl Duplicates IsPureUserFunc
**File**: `purity_adapter.go`

```go
// These two do exactly the same thing:
func (c *purityChecker) IsPureUserFunc(fn *ssa.Function) bool { ... }
func IsPureFunctionDecl(fn *ssa.Function, pureFuncs *directive.PureFuncSet) bool { ... }
```

---

### M3. O(n²) Algorithm in DetectReachabilityViolations
**File**: `pollution.go:398`

```go
for targetBlock, targetPos := range state.pollutedBlocks {
    for srcBlock := range state.pollutedBlocks {
```

All pairs checked for each polluted value.

---

### M4. trace() Method Too Complex (50+ lines)
**File**: `tracer.go:307`

Single method handles:
- Cycle detection
- Immutable checking
- Call processing
- MakeClosure/IIFE handling
- Method call handling
- Non-call node dispatching

**Recommendation**: Split into focused sub-methods.

---

### M5. CallHandler.Handle (70 lines)
**File**: `handler.go:77`

Too many responsibilities in one method. Should be split.

---

### M6. Suspicious Logic in IsPollutedAnywhere
**File**: `pollution.go:330`

```go
for pollutedBlock := range state.pollutedBlocks {
    if pollutedBlock.Parent() == fn {
        return true
    }
    return true  // ← Always returns true on first iteration
}
```

The loop always returns on first iteration. Bug or intentional?

---

### M7. Similar Trace Functions Without Abstraction
**File**: `tracer.go`

`TraceAllocStore`, `TraceFieldStore`, `TraceAllAllocStores` all follow the same pattern:
1. Get parent function
2. Loop through blocks
3. Loop through instructions
4. Filter by type
5. Check condition

Should be generalized with a `findStoresMatching(fn, predicate)` function.

---

### M8. PureFuncSet.Contains Parses Files
**File**: `directive/pure.go:44`

A `Contains()` method shouldn't trigger file parsing. Violates principle of least surprise.

---

### M9. Magic Number in Ignore Logic
**File**: `analyzer.go:51`

```go
ignoreMap.MarkUsed(fnLine - 1)
```

`fnLine - 1` assumes ignore comment is always on previous line. Should be a named constant or helper function.

---

### M10. TraceFreeVar Has O(n³) Potential
**File**: `tracer.go:83`

```go
for i, v := range fn.FreeVars {  // Loop 1
for _, block := range parent.Blocks {  // Loop 2
    for _, instr := range block.Instrs {  // Loop 3
```

---

### M11. Global Mutable Map
**File**: `typeutil/gorm.go:51`

```go
var ImmutableReturningMethods = map[string]struct{}{
```

Exported mutable map. Could be modified by external code.

---

### M12. HasSuffix for Package Path
**File**: `typeutil/gorm.go:40`

```go
strings.HasSuffix(obj.Pkg().Path(), gormPkgPath)
```

Could match `evil.com/fake-gorm.io/gorm`. Use exact match.

---

### M13. IsImmutableSource Incomplete
**File**: `tracer.go:446`

Only handles `*ssa.Parameter` and `*ssa.Call`. Missing:
- `*ssa.Const`
- `*ssa.Global`

---

### M14. Unexplained delete() in traceAll
**File**: `tracer.go:436`

```go
freshVisited := cloneVisited(visited)
delete(freshVisited, v)  // Why?
```

No comment explaining why self-deletion is needed.

---

### M15. Empty Case in AST Inspection
**File**: `directive/ignore.go:125`

```go
case *ast.FuncLit:
    // Function literals don't have doc comments in Go
    // But we can check for inline comments
```

Empty case. Either implement or remove.

---

## Minor Issues (Style/Cleanup)

### m1. Single-case switch statements
**File**: `handler.go:495`

```go
switch store.Addr.(type) {
case *ssa.IndexAddr:
    // ...
default:
    return
}
```

Should be `if _, ok := store.Addr.(*ssa.IndexAddr); !ok { return }`.

---

### m2. Comment explaining code that should be self-documenting
**File**: `analyzer.go:50`

```go
// The ignore comment is on the line before the function name
ignoreMap.MarkUsed(fnLine - 1)
```

The code needs a comment to explain itself = bad code.

---

### m3-m14. Various minor style issues
- Inconsistent error handling patterns
- Long parameter lists that could use structs
- Missing godoc on some exported functions
- Duplicate comments across files

---

## Recommendations

### Short-term (Quick Wins)
1. Extract duplicated pollution-checking logic to helper
2. Fix the `IsPollutedAnywhere` suspicious return
3. Add exact match for package path
4. Document the `traceAll` delete behavior

### Medium-term (Refactoring)
1. Replace Strategy pattern with type switch
2. Merge duplicate `processCallCommon` implementations
3. Split large methods (Handle, trace)
4. Remove or justify empty structs

### Long-term (Architecture)
1. Reconsider package structure (eliminate exports.go)
2. Add proper benchmarks before optimizing O(n²) algorithms
3. Consider state machine for pollution tracking

---

## Files Reviewed

| File | Lines | Issues |
|------|-------|--------|
| internal/analyzer.go | 266 | 4 |
| internal/exports.go | 78 | 3 |
| internal/purity_adapter.go | 52 | 2 |
| internal/ssa/handler.go | 621 | 8 |
| internal/ssa/tracer.go | 477 | 6 |
| internal/ssa/pollution.go | 478 | 4 |
| internal/directive/pure.go | 233 | 2 |
| internal/directive/ignore.go | 133 | 2 |
| internal/typeutil/gorm.go | 68 | 2 |
| internal/ssa/purity/inference.go | 524 | 1 (WIP) |
