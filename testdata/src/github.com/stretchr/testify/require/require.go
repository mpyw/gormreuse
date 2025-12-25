// Package require is a stub for testing purposes.
package require

// TestingT is an interface for testing.T
type TestingT interface {
	Errorf(format string, args ...interface{})
}

// NoError asserts that err is nil.
func NoError(t TestingT, err error, msgAndArgs ...interface{}) {
}
