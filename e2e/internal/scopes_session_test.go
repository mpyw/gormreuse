package internal

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests pin down which Session()-family calls inside a Scopes callback
// trigger go-gorm/gorm#7592 in the GORM version this module uses.
//
// gorm's processor runs the scopes first (db = scope(db)) and then runs the
// create/update/delete callbacks on the value the LAST scope returned.
// BeginTransaction stores its flag with db.InstanceSet, which calls
// getInstance. When that value has clone>0, getInstance clones the Statement,
// so the flag lands on a copy, CommitOrRollbackTransaction never sees it, and
// the transaction is never committed.
//
// Session/WithContext/Debug return a clone>0 value; every other chain method
// (Where, Model, ...) returns a clone==0 value. So the leak needs a
// Session-family result to BE the callback's return value. Where that value
// came from does not matter.

// scopedDeleteCommits runs a default-transaction Delete through scope and
// reports whether gorm committed the transaction it began.
func scopedDeleteCommits(t *testing.T, scope func(db, other *gorm.DB) func(*gorm.DB) *gorm.DB) bool {
	t.Helper()

	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("Failed to create sqlmock: %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec("DELETE").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      mockDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("Failed to open gorm: %v", err)
	}

	// other is a *gorm.DB unrelated to the callback parameter.
	other := db.Session(&gorm.Session{})
	res := db.Scopes(scope(db, other)).Where("id = ?", 1).Delete(&User{})
	if res.Error != nil {
		t.Fatalf("Delete returned error: %v", res.Error)
	}
	return mock.ExpectationsWereMet() == nil
}

func TestScopesSession_ReturnedSessionLeaks(t *testing.T) {
	tests := []struct {
		name  string
		scope func(db, other *gorm.DB) func(*gorm.DB) *gorm.DB
	}{
		{"Session", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{}) }
		}},
		{"WithContext", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB { return q.WithContext(context.Background()) }
		}},
		{"Debug", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{Logger: logger.Discard}).Debug() }
		}},
		{"chain then Session", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB { return q.Where("x = 1").Session(&gorm.Session{}) }
		}},
		{"Session result reused after a discarded chain", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB {
				s := q.Session(&gorm.Session{})
				_ = s.Where("x = 1")
				return s
			}
		}},
		{"Session on an unrelated DB", func(_, other *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(*gorm.DB) *gorm.DB {
				return other.Model(&User{}).Where("id = ?", 1).Session(&gorm.Session{})
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if scopedDeleteCommits(t, tt.scope) {
				t.Errorf("transaction committed; want the #7592 leak (no commit) when the callback returns a %s result", tt.name)
			}
		})
	}
}

func TestScopesSession_UnreturnedOrChainedSessionCommits(t *testing.T) {
	tests := []struct {
		name  string
		scope func(db, other *gorm.DB) func(*gorm.DB) *gorm.DB
	}{
		{"no Session", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB { return q.Where("x = 1") }
		}},
		{"Session followed by Where", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{}).Where("x = 1") }
		}},
		{"WithContext followed by Where", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB { return q.WithContext(context.Background()).Where("x = 1") }
		}},
		{"Session on an unrelated DB, not returned", func(_, other *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB {
				_ = other.Session(&gorm.Session{DryRun: true})
				return q
			}
		}},
		{"Session on the parameter, not returned", func(_, _ *gorm.DB) func(*gorm.DB) *gorm.DB {
			return func(q *gorm.DB) *gorm.DB {
				_ = q.Session(&gorm.Session{DryRun: true})
				return q
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !scopedDeleteCommits(t, tt.scope) {
				t.Errorf("transaction not committed; want a commit when the callback does not return a Session-family result (%s)", tt.name)
			}
		})
	}
}
