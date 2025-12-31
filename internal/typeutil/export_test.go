package typeutil

import "go/types"

// Export unexported functions for testing.

// IsGormDBNamed exports isGormDBNamed for external tests.
func IsGormDBNamed(t types.Type) bool {
	return isGormDBNamed(t)
}
