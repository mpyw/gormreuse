package determinism

// The unused-directive diagnostics come from map ranges, so without sorting
// their report order, and with it the -json output, changed between runs
// (#273). Several kinds in a second file make that show up within a few runs.

//gormreuse:pure // want `unused gormreuse:pure directive`
func unusedPure1(x int) int { return x }

//gormreuse:pure // want `unused gormreuse:pure directive`
func unusedPure2(x int) int { return x }

//gormreuse:pure // want `unused gormreuse:pure directive`
func unusedPure3(x int) int { return x }

//gormreuse:immutable-return // want `unused gormreuse:immutable-return directive`
func unusedImmutableReturn1() int { return 0 }

//gormreuse:immutable-return // want `unused gormreuse:immutable-return directive`
func unusedImmutableReturn2() int { return 0 }

//gormreuse:immutable-param // want `unused gormreuse:immutable-param directive`
func unusedImmutableParam1(x int) int { return x }

//gormreuse:immutable-param // want `unused gormreuse:immutable-param directive`
func unusedImmutableParam2(x int) int { return x }

func unusedIgnores() {
	_ = 1 //gormreuse:ignore // want `unused gormreuse:ignore directive`
	_ = 2 //gormreuse:ignore // want `unused gormreuse:ignore directive`
	_ = 3 //gormreuse:ignore // want `unused gormreuse:ignore directive`
}
