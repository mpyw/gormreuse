module github.com/mpyw/gormreuse

go 1.24.0

require golang.org/x/tools v0.40.0

require (
	golang.org/x/mod v0.31.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
)

// Retract all previous versions due to critical bugs:
// - v0.1.0-v0.8.0: Various issues with -test flag, pure directives, and immutability tracking
// - v0.9.0: Critical false positives in assignment detection:
//   * Conditional chain extension (q = q.Where() in if-block) incorrectly flagged as violation
//   * Loop+Phi patterns caused false positives in complex control flow
//   * Affected common GORM idioms in production codebases
retract [v0.1.0, v0.9.0]
