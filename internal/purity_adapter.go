package internal

import (
	"go/types"

	"github.com/mpyw/gormreuse/internal/purity"
	"golang.org/x/tools/go/ssa"
)

// purityChecker implements purity.PurityChecker using internal package functions.
type purityChecker struct {
	pureFuncs *PureFuncSet
}

// newPurityChecker creates a new purityChecker adapter.
func newPurityChecker(pureFuncs *PureFuncSet) purity.PurityChecker {
	return &purityChecker{pureFuncs: pureFuncs}
}

// IsGormDB implements purity.PurityChecker.
func (c *purityChecker) IsGormDB(t types.Type) bool {
	return IsGormDB(t)
}

// IsPureBuiltinMethod implements purity.PurityChecker.
func (c *purityChecker) IsPureBuiltinMethod(methodName string) bool {
	return IsPureFunctionBuiltin(methodName)
}

// IsPureUserFunc implements purity.PurityChecker.
func (c *purityChecker) IsPureUserFunc(fn *ssa.Function) bool {
	if c.pureFuncs == nil {
		return false
	}
	return c.pureFuncs.Contains(fn)
}
