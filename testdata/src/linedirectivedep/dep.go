// Package linedirectivedep holds a pure helper below a //line directive
// (issue #152). Callers in another package read it from the fact this
// package's analysis exports (#195).
package linedirectivedep

import "gorm.io/gorm"

//line dep.tmpl:1
//gormreuse:pure
func Helper(q *gorm.DB) {
	_ = q
}
