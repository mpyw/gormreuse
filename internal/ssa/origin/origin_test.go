package origin

import "testing"

// TestDeclared covers the nil case; the instance and method-value cases
// are in testdata/src/gormreuse/generic_directives.go (#180).
func TestDeclared(t *testing.T) {
	t.Parallel()
	if got := Declared(nil); got != nil {
		t.Errorf("Declared(nil) = %v, want nil", got)
	}
}
