# Refactoring Issues

This document tracks code quality issues, dead code, and improvement opportunities identified during code review.

**Date**: 2025-12-24
**Overall Coverage**: 85.2% (improved from 81.6%)

## Completed Actions (Session 2025-12-24)

### 1. Dead Code Removal

The following functions were identified as dead code and removed:

| Function | File | Reason |
|----------|------|--------|
| `inferInterfaceMethodCall` | `internal/ssa/purity/inference.go` | `*gorm.DB` is a concrete type, not an interface. `call.Call.Method` is never non-nil for concrete types. |
| `checkInterfaceMethodPollution` | `internal/ssa/purity/validator.go` | Same reason - interface method calls don't occur with `*gorm.DB`. |

**Explanation**: GORM's `DB` is a struct type, not an interface. When calling methods on `*gorm.DB`, SSA uses `StaticCallee()` (concrete type dispatch), not `call.Call.Method` (interface dispatch). The removed code was unreachable.

### 2. Test Coverage Improvements

Added test cases for Phi node handling:

- `pureWithPhiNode` - Tests SSA Phi node generation from variable assignment
- `purePhiWithDepends` - Tests Phi node merging with Depends state
- `purePhiWithPolluted` - Tests Phi node detection for Polluted return values

**Result**: `inferPhi` coverage improved from 0% to 87.5%

## Remaining Issues

### 1. Low Coverage Functions (< 50%)

| Function | Coverage | File | Issue |
|----------|----------|------|-------|
| `inferValueImpl` | 27.3% | `inference.go:99` | Many SSA types untested (FieldAddr, IndexAddr, Lookup, etc.) |
| `traceToParameterImpl` | 25.0% | `inference.go:385` | Phi/UnOp/ChangeType branches untested |

**Note**: These functions handle edge cases that rarely occur in real GORM usage. The untested branches are for SSA patterns like:
- `FieldAddr` - struct field containing `*gorm.DB`
- `IndexAddr` - slice/array element containing `*gorm.DB`
- `Lookup` - map value containing `*gorm.DB`

These patterns are supported for completeness but are not common in typical GORM code.

### 2. Package Dependency Coupling

```
internal/ssa/purity
├── internal/directive  (for PureFuncSet)
└── internal/typeutil
```

**Issue**: `purity` package depends on `directive` package for `PureFuncSet`. This couples SSA analysis logic with directive parsing.

**Recommendation**: Consider introducing an interface `PureFuncChecker`:

```go
// In purity package
type PureFuncChecker interface {
    Contains(fn *ssa.Function) bool
}

// directive.PureFuncSet would implement this interface
```

**Priority**: Low (current coupling is acceptable for project size)

### 3. Duplicated Logic

| Location 1 | Location 2 | Issue |
|------------|------------|-------|
| `isPureUserFunc` in `inference.go:293` | `isPureUserFunc` in `validator.go:144` | Identical implementations |

**Recommendation**: Extract to shared helper or use `PureFuncSet.Contains()` directly.

## Action Items for Next Session

### Priority 1: Improve Low Coverage Functions

- [ ] Consider removing untested branches in `inferValueImpl` if they're truly unreachable
- [ ] Add test cases for `traceToParameterImpl` edge cases if needed

### Priority 2: Eliminate Duplication

- [ ] Refactor `isPureUserFunc` to avoid duplication between inference.go and validator.go

### Priority 3: Consider Interface Extraction (Optional)

- [ ] Define `PureFuncChecker` interface if purity package needs to be reused independently

## Coverage Targets

- Initial: 81.6%
- Current: 85.2%
- Target: 90%+

## Session Notes (2025-12-24)

### Summary

| Metric | Before | After | Change |
|--------|--------|-------|--------|
| Coverage | 81.6% | 85.2% | +3.6% |
| 0% Coverage Functions | 3 | 0 | -3 |
| Dead Code Lines | ~50 | 0 | Removed |

### Actions Completed

1. **Coverage Analysis**
   - Identified 3 functions with 0% coverage: `inferInterfaceMethodCall`, `checkInterfaceMethodPollution`, `inferPhi`
   - Root cause: Interface method calls never occur with `*gorm.DB` (concrete type)

2. **Dead Code Removal**
   - Removed `inferInterfaceMethodCall` from `inference.go` (unreachable code)
   - Removed `checkInterfaceMethodPollution` from `validator.go` (unreachable code)
   - Added explanatory comments for why interface dispatch is not supported

3. **Test Improvements**
   - Added `pureWithPhiNode` - tests SSA Phi node generation
   - Added `purePhiWithDepends` - tests Phi node merging with Depends state
   - Added `purePhiWithPolluted` - tests Phi node with Polluted detection

4. **Static Analysis**
   - `golangci-lint`: 0 issues
   - All tests passing

### Files Modified

- `internal/ssa/purity/inference.go` - Removed `inferInterfaceMethodCall`
- `internal/ssa/purity/validator.go` - Removed `checkInterfaceMethodPollution`
- `testdata/src/gormreuse/pure_validation.go` - Added Phi node test cases
- `docs/REFACTORING_ISSUES.md` - Created this document
