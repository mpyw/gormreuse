package internal

import (
	"context"

	"gorm.io/gorm"
)

// =============================================================================
// TEMPORARY: Session/WithContext/Debug results returned from Scopes callbacks
// (GORM bug #7592). Issue #56 cases 3.1–3.4 and #196. These fixtures pair with
// internal/scopeswarn and should be deleted together once the upstream fix
// ships.
//
// gorm runs the create/update/delete callbacks on the value the scope RETURNS.
// The leak needs that value to be a Session-family result (clone>0); any other
// chain method after it returns a clone==0 value and is safe
// (e2e/internal/scopes_session_test.go).
// =============================================================================

// ===== SHOULD REPORT =====

// 3.1: a returned Session() result warns.
func scopesCallbackSessionWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
	})
}

// 3.2: a returned WithContext() result warns (it calls Session internally).
func scopesCallbackWithContextWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.WithContext(context.Background()) // want `WithContext\(\) result returned from Scopes callback causes transaction leak`
	})
}

// 3.3: a returned Debug() result warns (it calls Session internally).
func scopesCallbackDebugWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Where("x").Debug() // want `Debug\(\) result returned from Scopes callback causes transaction leak`
	})
}

// The Session result is stored, used, and then returned as it is.
func scopesCallbackStoredSessionWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		s := q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
		s.Where("a").Find(nil)
		return s
	})
}

// Session on a *gorm.DB unrelated to the parameter still leaks when returned.
func scopesCallbackUnrelatedSessionWarns(db, other *gorm.DB) {
	db.Scopes(func(*gorm.DB) *gorm.DB {
		return other.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
	})
}

// A captured Session result is read through the closure's FreeVar.
func scopesCallbackCapturedSessionWarns(db, other *gorm.DB) {
	s := other.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
	db.Scopes(func(*gorm.DB) *gorm.DB {
		return s
	})
}

// Only one branch returns the Session result.
func scopesCallbackConditionalSessionWarns(db *gorm.DB, debug bool) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		if debug {
			return q.Debug() // want `Debug\(\) result returned from Scopes callback causes transaction leak`
		}
		return q
	})
}

// #196: a named function passed to Scopes.
func namedSessionScope(q *gorm.DB) *gorm.DB {
	return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
}

func scopesNamedSessionWarns(db *gorm.DB) {
	db.Scopes(namedSessionScope)
}

// #196: a method value passed to Scopes.
type sessionScopeRepo struct{}

func (sessionScopeRepo) scope(q *gorm.DB) *gorm.DB {
	return q.WithContext(context.Background()) // want `WithContext\(\) result returned from Scopes callback causes transaction leak`
}

func scopesMethodValueSessionWarns(db *gorm.DB) {
	db.Scopes(sessionScopeRepo{}.scope)
}

// #196: a helper that the scope returns through.
func sessionScopeHelper(q *gorm.DB) *gorm.DB {
	return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
}

func scopesHelperSessionWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return sessionScopeHelper(q)
	})
}

// A helper that returns its argument passes the caller's Session result back.
func passThroughScopeHelper(q *gorm.DB) *gorm.DB {
	return q
}

func scopesPassThroughHelperSessionWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return passThroughScopeHelper(q.Debug()) // want `Debug\(\) result returned from Scopes callback causes transaction leak`
	})
}

// #196: a closure invoked inside the scope.
func scopesInvokedClosureSessionWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		wrap := func() *gorm.DB {
			return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
		}
		return wrap()
	})
}

// #196: a scope slice built with append.
func scopesAppendSliceSessionWarns(db *gorm.DB) {
	var scopes []func(*gorm.DB) *gorm.DB
	scopes = append(scopes, func(q *gorm.DB) *gorm.DB {
		return q.Debug() // want `Debug\(\) result returned from Scopes callback causes transaction leak`
	})
	db.Scopes(scopes...)
}

// #196: a package-level closure.
var packageSessionScope = func(q *gorm.DB) *gorm.DB {
	return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
}

func scopesPackageClosureSessionWarns(db *gorm.DB) {
	db.Scopes(packageSessionScope)
}

// A returned variable that may hold the Debug() result.
func scopesMergedSessionWarns(db *gorm.DB, debug bool) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		s := q
		if debug {
			s = q.Debug() // want `Debug\(\) result returned from Scopes callback causes transaction leak`
		}
		return s
	})
}

// #293: a method expression held in a package-level variable is named as
// the method it stands for.
var packageSessionExpr = (*gorm.DB).Session

func scopesPackageMethodExprSessionWarns(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return packageSessionExpr(q, &gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
	})
}

// ===== SHOULD NOT REPORT =====

// A chain method after Session returns a clone==0 value, so nothing leaks.
func scopesCallbackSessionThenWhereClean(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}).Where("x") // OK: Where returns a clone==0 value
	})
}

func scopesCallbackWithContextThenWhereClean(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.WithContext(context.Background()).Where("x") // OK: Where returns a clone==0 value
	})
}

func scopesCallbackDebugThenWhereClean(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Debug().Where("x") // OK: Where returns a clone==0 value
	})
}

// #196: Session on an unrelated *gorm.DB that is not returned does not reach
// the scope's execution.
func scopesCallbackUnreturnedSessionClean(db, other *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		other.Session(&gorm.Session{}).Find(nil) // OK: not the returned value
		return q
	})
}

// A helper's Session that the scope does not return.
func scopesHelperUnreturnedSessionClean(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return sessionScopeHelper(q).Where("x") // OK: Where returns a clone==0 value
	})
}

// 3.4: the same Session() inside a Preload callback is NOT flagged — Preload
// builds a fresh DB (NewDB) and is unaffected by the bug.
func preloadCallbackSessionClean(db *gorm.DB) {
	db.Preload("Rel", func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}) // OK: Preload callback is unaffected
	})
}

// The same function used outside Scopes is not a Scopes callback.
func notScopesSessionClean(q *gorm.DB) *gorm.DB {
	return q.Session(&gorm.Session{}) // OK: never passed to Scopes
}

// ===== THE FINISHER (#308) =====
//
// Only the create, update and delete callbacks begin a transaction, so a
// Scopes call whose chain provably ends in a finisher that reads does not
// leak (e2e/internal/scopes_finisher_test.go).

func scopesFinisherSession(q *gorm.DB) *gorm.DB {
	return q.Session(&gorm.Session{}) // OK: every Scopes call that passes it reads
}

func scopesFinisherReads(conn *gorm.DB) error {
	db := conn.Session(&gorm.Session{})
	var n int64
	db.Scopes(scopesFinisherSession).Find(nil)
	db.Scopes(scopesFinisherSession).Where("x").Order("y").Count(&n)
	q := db.Scopes(scopesFinisherSession)
	q.First(nil)
	db.Scopes(scopesFinisherSession).Debug().Pluck("name", nil)
	return db.Scopes(scopesFinisherSession).Find(nil).Error
}

func scopesFinisherWriteSession(q *gorm.DB) *gorm.DB {
	return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
}

// One Scopes call that writes is enough.
func scopesFinisherReadAndWrite(conn *gorm.DB) {
	db := conn.Session(&gorm.Session{})
	db.Scopes(scopesFinisherWriteSession).Find(nil)
	db.Scopes(scopesFinisherWriteSession).Where("x").Delete(nil)
}

func scopesFinisherUpdate(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.WithContext(context.Background()) // want `WithContext\(\) result returned from Scopes callback causes transaction leak`
	}).Update("name", "x")
}

// A chain that leaves the function may reach any finisher.
func scopesFinisherReturned(db *gorm.DB) *gorm.DB {
	return db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Debug() // want `Debug\(\) result returned from Scopes callback causes transaction leak`
	}).Where("x")
}

func scopesFinisherPassed(db *gorm.DB, use func(*gorm.DB)) {
	use(db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
	}))
}

// A finisher outside the read list, such as FirstOrCreate, may write.
func scopesFinisherFirstOrCreate(db *gorm.DB) {
	db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
	}).FirstOrCreate(nil)
}

// A Scopes result handed to another method is not its receiver.
func scopesFinisherArgument(conn *gorm.DB) {
	db := conn.Session(&gorm.Session{})
	sub := db.Scopes(func(q *gorm.DB) *gorm.DB {
		return q.Session(&gorm.Session{}) // want `Session\(\) result returned from Scopes callback causes transaction leak`
	})
	db.Where("id IN (?)", sub).Find(nil)
}
