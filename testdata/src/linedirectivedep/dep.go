// Package linedirectivedep holds a pure helper below a //line directive. The
// directive is read back by parsing this file from disk (issue #152).
package linedirectivedep

import "gorm.io/gorm"

//line dep.tmpl:1
//gormreuse:pure
func Helper(q *gorm.DB) {
	_ = q
}
