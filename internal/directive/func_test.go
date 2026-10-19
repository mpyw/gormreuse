package directive

import "testing"

// TestDeclaredFunc covers the nil case; the instance and method-value cases
// are in testdata/src/gormreuse/generic_directives.go (#180).
func TestDeclaredFunc(t *testing.T) {
	t.Parallel()
	if got := DeclaredFunc(nil); got != nil {
		t.Errorf("DeclaredFunc(nil) = %v, want nil", got)
	}
}
