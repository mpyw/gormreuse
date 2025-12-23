package internal

import (
	"go/types"

	"github.com/mpyw/gormreuse/internal/purity"
	"golang.org/x/tools/go/ssa"
)

// =============================================================================
// Purity Checker Adapter
// =============================================================================

// purityChecker implements purity.PurityChecker using internal package functions.
type purityChecker struct {
	pureFuncs *PureFuncSet
}

func newPurityChecker(pureFuncs *PureFuncSet) purity.PurityChecker {
	return &purityChecker{pureFuncs: pureFuncs}
}

func (c *purityChecker) IsGormDB(t types.Type) bool {
	return IsGormDB(t)
}

func (c *purityChecker) IsPureBuiltinMethod(methodName string) bool {
	return IsPureFunctionBuiltin(methodName)
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
func IsPureFunctionDecl(fn *ssa.Function, pureFuncs *PureFuncSet) bool {
	if pureFuncs == nil {
		return false
	}
	return pureFuncs.Contains(fn)
}
