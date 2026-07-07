package internal

import "gorm.io/gorm"

// =============================================================================
// Scopes/Preload callback parameters are MUTABLE roots (#60, Phase 1a)
//
// GORM passes the mid-chain (clone==0) *gorm.DB into Scopes/Preload callbacks,
// so reusing that parameter inside the callback interferes at runtime — exactly
// like reusing any other mutable *gorm.DB. This is the narrow, uncontroversial
// half of #56 Phase 1: only callbacks actually handed to Scopes/Preload are
// affected, not every func(*gorm.DB) *gorm.DB (that is Phase 1b).
// =============================================================================

// =============================================================================
// SHOULD REPORT
// =============================================================================

// SC001: Reuse of the Scopes callback parameter (two branches from tx).
func scopesCallbackReuse(db *gorm.DB) {
	db.Scopes(func(tx *gorm.DB) *gorm.DB {
		tx.Where("a").Find(nil) // first branch from tx
		return tx.Where("b")    // want `\*gorm\.DB reused: second branch from mutable root`
	})
}

// SC002: Reuse of a Preload scope callback parameter (a scope func rides in
// Preload's variadic args and receives the same mid-chain value).
func preloadCallbackReuse(db *gorm.DB) {
	db.Preload("Orders", func(tx *gorm.DB) *gorm.DB {
		tx.Where("a").Find(nil)
		return tx.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
	})
}

// SC003: A named function passed to Scopes is analyzed as an ordinary source
// function; its *gorm.DB parameter is a mutable root too.
func namedScope(tx *gorm.DB) *gorm.DB {
	tx.Where("a").Find(nil)
	return tx.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
}

func useNamedScope(db *gorm.DB) {
	db.Scopes(namedScope)
}

// =============================================================================
// SHOULD NOT REPORT
// =============================================================================

// SC101: Single use of the Scopes callback parameter is fine (one branch).
func scopesCallbackSingleUse(db *gorm.DB) {
	db.Scopes(func(tx *gorm.DB) *gorm.DB {
		return tx.Where("a") // OK: single branch from tx
	})
}

// SC102: Session() inside the callback isolates before branching.
func scopesCallbackSessionIsolated(db *gorm.DB) {
	db.Scopes(func(tx *gorm.DB) *gorm.DB {
		s := tx.Session(&gorm.Session{})
		s.Where("a").Find(nil)
		return s.Where("b") // OK: s is immutable (Session isolates)
	})
}

// SC103: A Transaction callback receives a FRESH (forkable, clone>0) tx, not a
// mid-chain value, so reuse inside it is safe and must NOT be reported. Only
// Scopes/Preload callbacks get the mutable-root treatment.
func transactionCallbackNoReport(db *gorm.DB) {
	_ = db.Transaction(func(tx *gorm.DB) error {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil) // OK: tx is a fresh session, not a Scopes callback
		return nil
	})
}

// SC104: Under Phase 1b (#61) an ordinary func(*gorm.DB) *gorm.DB helper's
// parameter is a mutable root — a caller may pass a mid-chain value, so branching
// it twice interferes. (Contrast with immutable-param-annotated helpers below.)
func ordinaryHelperParamBranches(tx *gorm.DB) *gorm.DB {
	tx.Where("a").Find(nil)
	return tx.Where("b") // want `\*gorm\.DB reused: second branch from mutable root`
}

// SC105: A Connection callback also receives a fresh (clone>0) handle, so reuse
// inside it is safe (#62).
func connectionCallbackNoReport(db *gorm.DB) {
	_ = db.Connection(func(tx *gorm.DB) error {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil) // OK: tx is a fresh handle
		return nil
	})
}

// SC106: A FindInBatches callback (the tx argument, third parameter of the
// method) likewise receives a fresh handle per batch (#62).
func findInBatchesCallbackNoReport(db *gorm.DB) {
	_ = db.FindInBatches(nil, 100, func(tx *gorm.DB, batch int) error {
		tx.Where("a").Find(nil)
		tx.Where("b").Find(nil) // OK: tx is a fresh handle
		return nil
	})
}
