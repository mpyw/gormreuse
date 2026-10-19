//declscope:shared // pure.go, immutable.go and fact.go run their work under it
package internal

import (
	"fmt"
	"os"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

// panicGuard runs per-function work and recovers from any panic, so that a
// single pathological function (exotic SSA the tracer mishandles) cannot abort
// the entire `go vet` run for the package. Aborting would be worse than any
// false positive and contrary to the conservative-bias design.
//
// A recovered panic is reported at the function as an internal error, once per
// function, so that the diagnostics it may have lost are not dropped silently
// (#186). The report goes through pass.Reportf directly, so no ignore directive
// suppresses it.
//
// Set GORMREUSE_DEBUG_PANIC to a non-empty value to re-panic instead, surfacing
// the stack trace for debugging.
type panicGuard struct {
	pass     *analysis.Pass
	reported map[*ssa.Function]bool
}

func (g *panicGuard) run(fn *ssa.Function, work func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if os.Getenv("GORMREUSE_DEBUG_PANIC") != "" {
			panic(fmt.Sprintf("gormreuse: panic analyzing %s: %v", fn, r))
		}
		if g.reported[fn] {
			return
		}
		g.reported[fn] = true
		g.pass.Reportf(fn.Pos(), "gormreuse: internal error analyzing %s: %v; please report this at https://github.com/mpyw/gormreuse/issues", fn.RelString(fn.Pkg.Pkg), r)
	}()
	work()
}
