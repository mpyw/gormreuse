// Package require is a stub for testing purposes.
package require

// TestingT is an interface wrapper around *testing.T
type TestingT interface {
	Errorf(format string, args ...interface{})
	FailNow()
}

// NoError asserts that a function returned no error (i.e. `nil`).
func NoError(t TestingT, err error, msgAndArgs ...interface{}) {
	if err != nil {
		t.Errorf("Received unexpected error:\n%+v", err)
		t.FailNow()
	}
}

// Error asserts that a function returned an error (i.e. not `nil`).
func Error(t TestingT, err error, msgAndArgs ...interface{}) {
	if err == nil {
		t.Errorf("An error is expected but got nil.")
		t.FailNow()
	}
}

// Equal asserts that two objects are equal.
func Equal(t TestingT, expected, actual interface{}, msgAndArgs ...interface{}) {
	// stub implementation
}

// NotNil asserts that the specified object is not nil.
func NotNil(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if object == nil {
		t.Errorf("Expected value not to be nil.")
		t.FailNow()
	}
}
