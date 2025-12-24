module github.com/mpyw/gormreuse

go 1.24.0

require golang.org/x/tools v0.40.0

require (
	golang.org/x/mod v0.31.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
)

// Retract all previous versions due to critical bugs:
// - v0.1.0-v0.2.0: -test flag conflicted with singlechecker's built-in flag
// - v0.3.0-v0.4.1: pure directive did not make return values immutable
// - v0.5.0: pure directive on methods failed due to fn.String() format mismatch
// - v0.6.0: pure directive in external packages was not detected
// - v0.7.0-v0.8.0: pure directive return values were not treated as immutable
retract [v0.1.0, v0.8.0]
