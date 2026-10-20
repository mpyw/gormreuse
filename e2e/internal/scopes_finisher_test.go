package internal

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests pin down which finishers go-gorm/gorm#7592 affects (#308).
//
// The leak is a transaction that BeginTransaction starts and
// CommitOrRollbackTransaction never ends. gorm registers those two callbacks
// only for create, update and delete. The query, row and raw callbacks begin
// no transaction, so a Scopes callback that returns a Session result is
// harmless for Find, Count and the other finishers that read. The Scopes
// warning (internal/scopeswarn) is withheld when every chain from the Scopes
// call provably ends in such a finisher.

// scopesFinisherMock opens gorm on a fresh sqlmock with the default
// transaction behavior.
func scopesFinisherMock(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("Failed to create sqlmock: %v", err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      mockDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("Failed to open gorm: %v", err)
	}
	return db, mock
}

// scopesFinisherSession is a Scopes callback that returns a Session result.
func scopesFinisherSession(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{}) }

func TestScopesFinisher_ReadsDoNotLeak(t *testing.T) {
	tests := []struct {
		name  string
		query string
		col   string
		run   func(db *gorm.DB) error
	}{
		{"Find", "SELECT \\* FROM `users` WHERE id = \\?", "id", func(db *gorm.DB) error {
			var users []User
			return db.Scopes(scopesFinisherSession).Where("id = ?", 1).Find(&users).Error
		}},
		{"First", "SELECT \\* FROM `users` WHERE id = \\?", "id", func(db *gorm.DB) error {
			var u User
			return db.Scopes(scopesFinisherSession).Where("id = ?", 1).First(&u).Error
		}},
		{"Count", "SELECT count\\(\\*\\) FROM `users`", "count(*)", func(db *gorm.DB) error {
			var n int64
			return db.Scopes(scopesFinisherSession).Model(&User{}).Count(&n).Error
		}},
		{"Pluck", "SELECT `name` FROM `users`", "name", func(db *gorm.DB) error {
			var names []string
			return db.Scopes(scopesFinisherSession).Model(&User{}).Pluck("name", &names).Error
		}},
		{"Scan", "SELECT .*id.* FROM `users`", "id", func(db *gorm.DB) error {
			var ids []int
			return db.Scopes(scopesFinisherSession).Model(&User{}).Select("id").Scan(&ids).Error
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock := scopesFinisherMock(t)
			// No ExpectBegin: a transaction would fail the query.
			mock.ExpectQuery(tt.query).WillReturnRows(sqlmock.NewRows([]string{tt.col}).AddRow(1))
			if err := tt.run(db); err != nil {
				t.Fatalf("%s returned error: %v", tt.name, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("%s: %v", tt.name, err)
			}
		})
	}
}

func TestScopesFinisher_ExecDoesNotLeak(t *testing.T) {
	db, mock := scopesFinisherMock(t)
	mock.ExpectExec("UPDATE users SET name = \\?").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := db.Scopes(scopesFinisherSession).Exec("UPDATE users SET name = ?", "a").Error; err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestScopesFinisher_WritesLeak(t *testing.T) {
	tests := []struct {
		name string
		exec string
		run  func(db *gorm.DB) error
	}{
		{"Create", "INSERT INTO `users`", func(db *gorm.DB) error {
			return db.Scopes(scopesFinisherSession).Create(&User{Name: "a"}).Error
		}},
		{"Update", "UPDATE `users` SET `name`=\\?", func(db *gorm.DB) error {
			return db.Scopes(scopesFinisherSession).Model(&User{}).Where("id = ?", 1).Update("name", "a").Error
		}},
		{"Delete", "DELETE FROM `users`", func(db *gorm.DB) error {
			return db.Scopes(scopesFinisherSession).Where("id = ?", 1).Delete(&User{}).Error
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock := scopesFinisherMock(t)
			mock.ExpectBegin()
			mock.ExpectExec(tt.exec).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()
			if err := tt.run(db); err != nil {
				t.Fatalf("%s returned error: %v", tt.name, err)
			}
			if mock.ExpectationsWereMet() == nil {
				t.Errorf("%s committed its transaction; want the #7592 leak (no commit)", tt.name)
			}
		})
	}
}
