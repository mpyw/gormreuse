package debug

import (
	"go/token"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/pollution"
)

var _ pollution.DebugCollector = (*Tracker)(nil)

// Tracker wraps pollution.Tracker and adds debug information collection.
type Tracker struct {
	pollution.Tracker
	collector          *Collector
	enrichedViolations []Violation
}

// NewTracker creates a debug-enabled tracker that wraps a base tracker.
func NewTracker(base pollution.Tracker) *Tracker {
	return &Tracker{
		Tracker:   base,
		collector: NewCollector(),
	}
}

// CollectDebugInfo implements pollution.DebugCollector interface.
func (dt *Tracker) CollectDebugInfo(root ssa.Value, pos token.Pos, methodName, callType string) {
	dt.collector.RecordUsage(root, pos, methodName, callType)
}

// DetectViolations wraps the base DetectViolations and enriches violations with debug info.
func (dt *Tracker) DetectViolations() {
	// Run base detection
	dt.Tracker.DetectViolations()

	// Get all violations from base
	baseViolations := dt.Tracker.CollectViolations()

	// Enrich each violation with debug info
	dt.enrichedViolations = make([]Violation, 0, len(baseViolations))
	for _, v := range baseViolations {
		dt.enrichedViolations = append(dt.enrichedViolations, &violation{
			pos:       v.Pos(),
			message:   v.Message(),
			debugInfo: dt.collector.BuildViolationInfoByPos(v.Pos()),
		})
	}
}

// CollectViolations returns debug-enriched violations.
func (dt *Tracker) CollectViolations() []pollution.Violation {
	result := make([]pollution.Violation, len(dt.enrichedViolations))
	for i, v := range dt.enrichedViolations {
		result[i] = v
	}
	return result
}
