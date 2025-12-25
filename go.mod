module github.com/mpyw/gormreuse

go 1.24.0

require golang.org/x/tools v0.40.0

require (
	golang.org/x/mod v0.31.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
)

// Retract all previous versions due to critical bugs:
// - v0.1.0-v0.8.0: Various issues with -test flag, pure directives, and immutability tracking
// - v0.9.0: Critical false positives in assignment detection
// - v0.10.0-v0.10.2: Duplicate diagnostics in closures and inappropriate fix generation
//   * Same violation reported multiple times when closures access parent scope variables
//   * Fix generator produced invalid code like "tx = require.NoError(...)" for non-GORM wrappers
retract [v0.1.0, v0.10.2]
