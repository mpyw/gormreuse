// Package effort counts the steps of the analyzer's searches: a value traced, a
// block visited, a store or a referrer read. The allocation-growth gate reads
// the count, so a search that allocates nothing, such as a linear scan
// repeated in a loop, is gated too (#350). Counting is off until a test turns
// it on, so the analyzer pays one atomic load per step.
package effort

import "sync/atomic"

var (
	counting atomic.Bool
	steps    atomic.Uint64
)

// Step counts one step of a search, while counting is on.
func Step() {
	if counting.Load() {
		steps.Add(1)
	}
}

// Count turns counting on and returns the steps counted so far. The count is
// process-wide, so a test that reads it runs the analyzer on one package at a
// time.
func Count() uint64 {
	counting.Store(true)
	return steps.Load()
}
