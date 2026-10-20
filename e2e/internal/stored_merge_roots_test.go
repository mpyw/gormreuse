package internal

import (
	"context"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
)

// smrSerialGroup runs each function in its own goroutine, as errgroup.Group
// does, but waits for it before the next one starts, as SetLimit(1) does.
// The order of the queries is then fixed, and the test has no data race.
type smrSerialGroup struct{ wg sync.WaitGroup }

func (g *smrSerialGroup) Go(f func() error) {
	g.wg.Go(func() { _ = f() })
	g.wg.Wait()
}

// smrPage is the query of #340: two queries in goroutines that capture q,
// the first with a condition of its own. isolate ends the conditional chain
// with a Session. (A Count against the mock fails to scan its row, and the
// error, not a clause, would then reach the second query.)
func smrPage(db *gorm.DB, name string, isolate bool) {
	q := db.WithContext(context.Background())
	if name != "" {
		q = q.Where("name LIKE ?", name)
		if isolate {
			q = q.Session(&gorm.Session{})
		}
	}
	var g smrSerialGroup
	g.Go(func() error { return q.Where("active = ?", true).Find(&[]User{}).Error })
	g.Go(func() error { return q.Limit(10).Find(&[]User{}).Error })
}

// TestStoredMergeRootErrgroupLeak shows the leak that #340 reports: with the
// filter set, q holds the Where chain, so the first query adds its condition
// to the Statement that the second one reads. The analyzer missed it because the first
// value stored into q, the WithContext result, is immutable.
func TestStoredMergeRootErrgroupLeak(t *testing.T) {
	db, captured := setupDB(t)
	smrPage(db, "a%", false)
	if len(*captured) != 2 {
		t.Fatalf("want 2 queries, got %q", *captured)
	}
	if find := (*captured)[1]; !strings.Contains(find, "active") {
		t.Errorf("the second query did not read the first one's condition: %s", find)
	}
}

// TestStoredMergeRootErrgroupIsolated checks the fix: with a Session at the
// end of the chain, each goroutine builds its query on its own copy.
func TestStoredMergeRootErrgroupIsolated(t *testing.T) {
	db, captured := setupDB(t)
	smrPage(db, "a%", true)
	if len(*captured) != 2 {
		t.Fatalf("want 2 queries, got %q", *captured)
	}
	if find := (*captured)[1]; strings.Contains(find, "active") || !strings.Contains(find, "name LIKE") {
		t.Errorf("the second query read the first one's condition or lost the filter: %s", find)
	}
}
