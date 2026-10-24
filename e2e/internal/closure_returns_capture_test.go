package internal

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// closureRun calls fn and chains on the value it returns, as Scopes and
// Preload do with their callbacks (#397).
func closureRun(fn func() *gorm.DB) {
	fn().Where("age > ?", 18).Find(&[]User{})
}

// TestClosureReturnsCaptureLeak shows the reuse that #397 reports: a function
// literal that returns q hands q on, and closureRun's condition reaches the
// later query on q.
func TestClosureReturnsCaptureLeak(t *testing.T) {
	find := handoffSecondQuery(t, func(db *gorm.DB) {
		q := db.Where("name = ?", "x")
		closureRun(func() *gorm.DB { return q })
		q.Find(&[]User{})
	})
	if !strings.Contains(find, "age >") {
		t.Errorf("the later query did not read the condition added through the literal: %s", find)
	}
}

// TestClosureReturnsCaptureIsolated checks that a Session at q's root keeps
// closureRun's condition out of the later query.
func TestClosureReturnsCaptureIsolated(t *testing.T) {
	find := handoffSecondQuery(t, func(db *gorm.DB) {
		q := db.Where("name = ?", "x").Session(&gorm.Session{})
		closureRun(func() *gorm.DB { return q })
		q.Find(&[]User{})
	})
	if strings.Contains(find, "age >") || !strings.Contains(find, "name =") {
		t.Errorf("the later query read the literal's condition or lost its own: %s", find)
	}
}
