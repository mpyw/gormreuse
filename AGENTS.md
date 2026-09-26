# Repository instructions

gormreuse is a Go SSA analyzer for unsafe reuse of mutable `*gorm.DB` chains. The analysis model, limitations, rejected approaches, and test strategy are in [implementation notes](design/implementation.md). Read the relevant section before changing detection or fixes.

Preserve conservative reporting under uncertain control flow. Run `go test ./...` for Go changes and `./test_all.sh` before claiming the full repository gate passes. Keep user-facing behavior in the README aligned with the analyzer.
