# Refactoring Issues

This document tracks code quality issues, dead code, and improvement opportunities identified during code review.

**Date**: 2025-12-24
**Overall Coverage**: 88.2% (improved from 81.6%)

## Completed Actions (Session 2025-12-24)

### 1. Dead Code Removal

The following functions were identified as dead code and removed:

| Function | File | Reason |
|----------|------|--------|
| `inferInterfaceMethodCall` | `internal/ssa/purity/inference.go` | `*gorm.DB` is a concrete type, not an interface. `call.Call.Method` is never non-nil for concrete types. |
| `checkInterfaceMethodPollution` | `internal/ssa/purity/validator.go` | Same reason - interface method calls don't occur with `*gorm.DB`. |

**Explanation**: GORM's `DB` is a struct type, not an interface. When calling methods on `*gorm.DB`, SSA uses `StaticCallee()` (concrete type dispatch), not `call.Call.Method` (interface dispatch). The removed code was unreachable.

### 2. Duplication Elimination

| Removed | From | Reason |
|---------|------|--------|
| `isPureUserFunc` method | `inference.go:293` | Replaced with direct `pureFuncs.Contains()` call |
| `isPureUserFunc` method | `validator.go:144` | Replaced with direct `pureFuncs.Contains()` call |

`PureFuncSet.Contains()` already handles nil receiver safely, making the wrapper methods unnecessary.

### 3. Test Coverage Improvements

#### Phi Node Tests
- `pureWithPhiNode` - Tests SSA Phi node generation from variable assignment
- `purePhiWithDepends` - Tests Phi node merging with Depends state
- `purePhiWithPolluted` - Tests Phi node detection for Polluted return values

**Result**: `inferPhi` coverage improved from 0% to 87.5%

#### Direct Return Tests (inferValueImpl coverage)
- `pureReturnsDeref` - Tests UnOp (pointer dereference)
- `pureReturnsStructField` - Tests FieldAddr (struct field access)
- `pureReturnsSliceElement` - Tests IndexAddr (slice element)
- `pureReturnsMapValue` - Tests Lookup (map value)
- `pureReturnsTypeAssertDirect` - Tests TypeAssert
- `pureReturnsChangeType` - Tests ChangeType (type alias)
- `pureWithNonGormParam` - Tests Parameter branch for non-gorm.DB types

**Result**: `inferValueImpl` coverage improved from 27.3% to 54.5%

#### Trace to Parameter Tests (traceToParameterImpl coverage)
- `pureCallsWithDeref` - Tests UnOp trace through pure function call
- `pureCallsWithPhi` - Tests Phi node trace with same parameter
- `pureCallsWithDifferentParams` - Tests Phi trace failure with different parameters

**Result**: `traceToParameterImpl` coverage improved from 25.0% to 80.0%

## Remaining Issues

### 1. Low Coverage Functions

| Function | Coverage | File | Note |
|----------|----------|------|------|
| `inferValueImpl` | 54.5% | `inference.go:99` | Remaining branches are rare SSA patterns |

**Note**: The remaining uncovered branches handle edge cases that rarely occur in real GORM usage:
- `Extract` - Multiple return values (GORM methods return single values)
- `MakeClosure` - Closure returning `*gorm.DB` type
- `ChangeType`, `Convert` - Type conversions (rare with pointer types)
- `MakeInterface`, `Slice` - Interface wrapping and slice operations

These are kept for defensive completeness but may be considered for removal if truly unreachable.

### 2. Package Dependency Coupling

```
internal/ssa/purity
├── internal/directive  (for PureFuncSet)
└── internal/typeutil
```

**Status**: Acceptable for project size. The `isPureUserFunc` duplication was eliminated by using `PureFuncSet.Contains()` directly.

## Coverage Progress

| Stage | Coverage | Change |
|-------|----------|--------|
| Initial | 81.6% | - |
| After dead code removal | 85.2% | +3.6% |
| After duplication removal | 86.3% | +1.1% |
| After SSA type coverage | 87.7% | +1.4% |
| After inference improvements | 88.2% | +0.5% |
| **Target** | 90%+ | - |

## Session Notes (2025-12-24)

### Summary

| Metric | Before | After | Change |
|--------|--------|-------|--------|
| Coverage | 81.6% | 88.2% | +6.6% |
| 0% Coverage Functions | 3 | 0 | -3 |
| Dead Code Lines | ~50 | 0 | Removed |
| Duplicated Methods | 2 | 0 | Eliminated |

### Actions Completed

1. **Coverage Analysis**
   - Identified 3 functions with 0% coverage: `inferInterfaceMethodCall`, `checkInterfaceMethodPollution`, `inferPhi`
   - Root cause: Interface method calls never occur with `*gorm.DB` (concrete type)

2. **Dead Code Removal**
   - Removed `inferInterfaceMethodCall` from `inference.go` (unreachable code)
   - Removed `checkInterfaceMethodPollution` from `validator.go` (unreachable code)
   - Added explanatory comments for why interface dispatch is not supported

3. **Duplication Elimination**
   - Removed `isPureUserFunc` from both `inference.go` and `validator.go`
   - Replaced with direct `pureFuncs.Contains()` calls

4. **Test Improvements**
   - Added Phi node test cases (3 tests)
   - Added direct return test cases (8 tests)
   - Added trace-to-parameter test cases (3 tests)

5. **Static Analysis**
   - `golangci-lint`: 0 issues
   - All tests passing

### Files Modified

- `internal/ssa/purity/inference.go` - Removed dead code and `isPureUserFunc`
- `internal/ssa/purity/validator.go` - Removed dead code and `isPureUserFunc`
- `testdata/src/gormreuse/pure_validation.go` - Added comprehensive test cases
- `docs/REFACTORING_ISSUES.md` - Updated documentation
