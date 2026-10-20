package ignoretests

import "testing"

func TestIgnoreTestsUse(t *testing.T) {
	r := &ignoreTestsRepo{}
	r.db = r.db.Where("x")
	ignoreTestsUse(r)
}
