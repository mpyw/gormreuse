package cfg

import (
	"golang.org/x/tools/go/ssa"
)

// Repeats answers whether a use of a value may run more than once for one
// run of the instruction that defines the value. A use that does reads the
// same value again, so a mutable root used there is reused even with a
// single use in the source.
//
// It is the one place the analyzer asks that question for the uses it
// records. Today it holds the loops of the function that is analysed, so it
// sees only that function's loops.
type Repeats struct {
	loops *LoopInfo
}

// NewRepeats returns the Repeats of a function whose loops are loops.
func NewRepeats(loops *LoopInfo) *Repeats {
	return &Repeats{loops: loops}
}

// RepeatsAfter reports whether a use in block use may run more than once for
// one run of def, the block that defines the value (see tracer.RootBlock).
// That is so when use is in a loop and def is not:
//
//	q := db.Where("x")        // def, outside the loop
//	for _, item := range items {
//	    q.Where(item).Find(nil)  // use, runs again on each iteration
//	}
//
// A nil def, such as that of a global, is outside the loop. So is a def in
// another function, such as the function that declares a closure whose body
// is the use's: only the use's function's loops are known. A def in a loop
// is taken to run as often as the use, whichever loop it is in.
func (r *Repeats) RepeatsAfter(use, def *ssa.BasicBlock) bool {
	return r.loops.IsInLoop(use) && r.loops.IsDefinedOutsideLoop(def)
}
