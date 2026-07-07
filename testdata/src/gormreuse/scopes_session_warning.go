package internal

import (
	"context"

	"gorm.io/gorm"
)

// =============================================================================
// TEMPORARY: Session/WithContext/Debug inside Scopes callbacks (GORM bug #7592)
// Issue #56 cases 3.1–3.4. These fixtures pair with internal/scopes_session_warning.go
// and should be deleted together once the upstream fix ships.
// =============================================================================

// 3.1: Session() inside a Scopes callback warns.
func scopesCallbackSessionWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}).Where("x") // want `Session\(\) in Scopes callback causes transaction leak`
	})
}

// 3.2: WithContext() inside a Scopes callback warns (it calls Session internally).
func scopesCallbackWithContextWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.WithContext(context.Background()).Where("x") // want `WithContext\(\) in Scopes callback causes transaction leak`
	})
}

// 3.3: Debug() inside a Scopes callback warns (it calls Session internally).
func scopesCallbackDebugWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Debug().Where("x") // want `Debug\(\) in Scopes callback causes transaction leak`
	})
}

// 3.4: the same Session() inside a Preload callback is NOT flagged — Preload
// builds a fresh DB (NewDB) and is unaffected by the bug.
func preloadCallbackSessionClean(db *gorm.DB) {
	db.Preload("Rel", func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}).Where("x") // OK: Preload callback is unaffected
	})
}
