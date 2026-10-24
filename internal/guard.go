//declscope:shared // pure.go, immutable.go and fact.go run their work under it
package internal

import (
	"os"
	"runtime/debug"

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
// The work of a caller that sees the panic must not be trusted: run and
// runPackage report whether the work finished, and the caller treats a
// function whose work did not as breaking its contract (#315).
//
// Set GORMREUSE_DEBUG_PANIC to a non-empty value to add the stack trace to the
// report, for debugging.
type panicGuard struct {
	pass     *analysis.Pass
	reported map[*ssa.Function]bool
	// packageFailed is set once runPackage recovered a panic.
	//
	//declscope:private
	packageFailed bool
}

// guardPanicInjection, when set, runs at the start of the work of every stage
// (with fn nil for runPackage), so that a test can make that work panic. It
// is nil outside tests.
//
//declscope:private
var guardPanicInjection func(stage string, fn *ssa.Function)

// run runs the work of stage for fn and reports whether it finished without
// a panic.
func (g *panicGuard) run(stage string, fn *ssa.Function, work func()) (finished bool) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if g.reported[fn] {
			return
		}
		g.reported[fn] = true
		g.pass.Reportf(fn.Pos(), "gormreuse: internal error analyzing %s: %v; please report this at https://github.com/mpyw/gormreuse/issues%s", fn.RelString(fn.Pkg.Pkg), r, guardStack())
	}()
	if guardPanicInjection != nil {
		guardPanicInjection(stage, fn)
	}
	work()
	return true
}

// runPackage runs the work of stage, which reads the whole package, such as
// collecting the callbacks, and reports whether it finished without a panic.
// The report names stage, at the package clause of the first file.
func (g *panicGuard) runPackage(stage string, work func()) (finished bool) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		g.packageFailed = true
		g.pass.Reportf(g.pass.Files[0].Package, "gormreuse: internal error in %s of package %s: %v; please report this at https://github.com/mpyw/gormreuse/issues%s", stage, g.pass.Pkg.Path(), r, guardStack())
	}()
	if guardPanicInjection != nil {
		guardPanicInjection(stage, nil)
	}
	work()
	return true
}

// panicked reports whether any work recovered a panic, so that diagnostics
// may have been lost.
func (g *panicGuard) panicked() bool {
	return g.packageFailed || len(g.reported) > 0
}

// guardStack is the stack trace to add to a report, starting with a newline,
// when GORMREUSE_DEBUG_PANIC is set, and empty otherwise. Called while a
// recovered panic unwinds, it holds the frames that panicked.
//
//declscope:private
func guardStack() string {
	if os.Getenv("GORMREUSE_DEBUG_PANIC") == "" {
		return ""
	}
	return "\n" + string(debug.Stack())
}
