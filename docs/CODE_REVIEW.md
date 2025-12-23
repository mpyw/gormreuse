# Code Review - Tech Lead Review

Date: 2025-12-23 (Updated: 2025-12-24)
Reviewer: Claude (Tech Lead perspective)
Status: **34 issues identified, 15 fixed**

---

## Summary

| Severity | Count | Fixed |
|----------|-------|-------|
| Critical (Design Issues) | 5 | 1 |
| Major (Code Quality) | 15 | 10 |
| Minor (Style/Cleanup) | 14 | 4 |

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

### C2. ~~Strategy Pattern Overhead Without Benefit~~ (FIXED)
**File**: `handler.go`, `analyzer.go`

~~O(n) dispatch for every instruction. With 6 handlers, this means 6 type checks per instruction.~~

**Fixed**: Replaced with type switch in `DispatchInstruction`. Removed `InstructionHandler` interface and `CanHandle()` methods.

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

### M1. ~~Massive Code Duplication in Handlers~~ (FIXED)
**File**: `handler.go`

~~`GoHandler.processCallCommon` and `DeferHandler.processCallCommon` are nearly identical.~~

**Fixed**: Common logic extracted to `processGormDBCallCommon`. `CallHandler` pollution logic extracted to `checkAndMarkPollution` method on `HandlerContext`.

---

### M2. ~~IsPureFunctionDecl Duplicates IsPureUserFunc~~ (FIXED)
**File**: `purity_adapter.go`

~~These two do exactly the same thing.~~

**Fixed**: Removed `IsPureFunctionDecl`. Inlined `pureFuncs != nil && pureFuncs.Contains(fn)` in analyzer.go.

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

### M6. ~~Suspicious Logic in IsPollutedAnywhere~~ (FIXED)
**File**: `pollution.go:330`

~~The loop always returns on first iteration. Bug or intentional?~~

**Fixed**: Simplified to `return state != nil && len(state.pollutedBlocks) > 0`. Conservative approach documented.

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

### M9. ~~Magic Number in Ignore Logic~~ (FIXED)
**File**: `analyzer.go:51`

~~`fnLine - 1` assumes ignore comment is always on previous line.~~

**Fixed**: `BuildFunctionIgnoreSet` now returns `FunctionIgnoreEntry` with the actual directive line number. No more magic number assumptions.

---

### M10. TraceFreeVar Has O(n³) Potential
**File**: `tracer.go:83`

```go
for i, v := range fn.FreeVars {  // Loop 1
for _, block := range parent.Blocks {  // Loop 2
    for _, instr := range block.Instrs {  // Loop 3
```

---

### M11. ~~Global Mutable Map~~ (FIXED)
**File**: `typeutil/gorm.go:51`

~~Exported mutable map. Could be modified by external code.~~

**Fixed**: Renamed to `immutableReturningMethods` (unexported). External code now uses `IsPureFunctionBuiltin()` function.

---

### M12. ~~HasSuffix for Package Path~~ (FIXED)
**File**: `typeutil/gorm.go:40`

~~Could match `evil.com/fake-gorm.io/gorm`. Use exact match.~~

**Fixed**: Changed to exact match: `obj.Pkg().Path() == gormPkgPath`.

---

### M13. ~~IsImmutableSource Incomplete~~ (PARTIALLY FIXED)
**File**: `tracer.go:446`

~~Only handles `*ssa.Parameter` and `*ssa.Call`.~~

**Fixed**: Added `*ssa.Const` case (constants including nil are immutable). `*ssa.Global` not added as globals can be mutable.

---

### M14. ~~Unexplained delete() in traceAll~~ (FIXED)
**File**: `tracer.go:436`

~~No comment explaining why self-deletion is needed.~~

**Fixed**: Added explanatory comment: "Clone visited and remove current value to allow trace() to re-process it."

---

### M15. ~~Empty Case in AST Inspection~~ (FIXED)
**File**: `directive/ignore.go:125`

~~Empty case with misleading comment.~~

**Fixed**: Removed empty switch case. Simplified to type assertion with early return.

---

## Minor Issues (Style/Cleanup)

### m1. ~~Single-case switch statements~~ (FIXED)
**File**: `handler.go:495`

~~Should be `if _, ok := store.Addr.(*ssa.IndexAddr); !ok { return }`.~~

**Fixed**: Converted to if statement.

---

### m2. ~~Comment explaining code that should be self-documenting~~ (FIXED)
**File**: `analyzer.go:50`

~~The code needs a comment to explain itself = bad code.~~

**Fixed**: Refactored to use `FunctionIgnoreEntry.DirectiveLine` which is self-explanatory.

---

### m3. ~~Generic Helper Name `IsNilConst`~~ (FIXED)
**File**: `tracer.go`

~~Exported helper function with generic name that could collide with other packages.~~

**Fixed**: Renamed to `isNilConst` (unexported). Internal implementation detail, not needed by external callers.

---

### m4. ~~`IsFunctionDescendantOf` exported but only used internally~~ (FIXED)
**File**: `pollution.go`

~~Exported function with generic name, only used within the same package.~~

**Fixed**: Renamed to `isFunctionDescendantOf` (unexported). Only used by `DetectReachabilityViolations` in the same file.

---

### m5. ~~`ClosureCapturesGormDB` misplaced in handler.go~~ (FIXED)
**File**: `handler.go` → `tracer.go`

~~Type-checking utility function placed in instruction handler file. Violates single responsibility.~~

**Fixed**: Moved to `tracer.go` under Helper Functions section. SSA value analysis is closer to RootTracer's responsibility.

---

### m6-m14. Various minor style issues
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
