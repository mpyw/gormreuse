package purelib

import "gorm.io/gorm"

// Hooks run the callbacks registered earlier. A caller that stores a chain
// into a package-level variable cannot tell what RunHooks runs (#404).

var hooks []func()

// HooksConn makes no difference to the hooks. It only shows that this
// package reaches gorm.
var HooksConn *gorm.DB

// RegisterHook adds a callback.
func RegisterHook(f func()) { hooks = append(hooks, f) }

// RunHooks calls every callback.
func RunHooks() {
	for _, f := range hooks {
		f()
	}
}
