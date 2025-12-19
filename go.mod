module github.com/mpyw/gormreuse

go 1.24.0

require golang.org/x/tools v0.40.0

require (
	golang.org/x/mod v0.31.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
)

// Retract all previous versions due to -test flag not working
// (conflicted with singlechecker's built-in flag)
retract [v0.1.0, v0.2.0]
