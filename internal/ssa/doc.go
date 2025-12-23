// Package ssa provides SSA-based analysis for GORM *gorm.DB reuse detection.
//
// The package contains:
//   - SSATracer: Common SSA value traversal patterns (Phi, FreeVar, Alloc, etc.)
//   - RootTracer: Traces SSA values to find mutable *gorm.DB roots
//   - CFGAnalyzer: Control flow graph analysis (loop detection, reachability)
//   - PollutionTracker: Tracks pollution state of *gorm.DB values
//   - Handlers: Type-specific handlers dispatched via DispatchInstruction
//
// Architecture follows mechanism vs policy separation:
//   - SSATracer: HOW to traverse SSA values (mechanism)
//   - RootTracer: WHAT constitutes a mutable root (policy)
package ssa
