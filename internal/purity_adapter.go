package internal

import (
	"go/types"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// =============================================================================
// Purity Checker Adapter
// =============================================================================

// purityChecker implements purity.PurityChecker using internal package functions.
type purityChecker struct {
	pureFuncs *directive.PureFuncSet
}

func newPurityChecker(pureFuncs *directive.PureFuncSet) purity.PurityChecker {
	return &purityChecker{pureFuncs: pureFuncs}
}

func (c *purityChecker) IsGormDB(t types.Type) bool {
	return typeutil.IsGormDB(t)
}

func (c *purityChecker) IsPureBuiltinMethod(methodName string) bool {
	return typeutil.IsPureFunctionBuiltin(methodName)
}

func (c *purityChecker) IsPureUserFunc(fn *ssa.Function) bool {
	if c.pureFuncs == nil {
		return false
	}
	return c.pureFuncs.Contains(fn)
}

// =============================================================================
// Pure Function Declaration Check
// =============================================================================

// IsPureFunctionDecl checks if a function declaration has a pure directive.
func IsPureFunctionDecl(fn *ssa.Function, pureFuncs *directive.PureFuncSet) bool {
	if pureFuncs == nil {
		return false
	}
	return pureFuncs.Contains(fn)
}
