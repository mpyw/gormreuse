package internal

import (
	"slices"

	"gorm.io/gorm"
)

// =============================================================================
// Reads from a container (#374)
//
// A *gorm.DB read from a slice, an array, a map or a channel, or through a
// pointer parameter, is a root of its own (tracer.ElemRoot) when the container
// may hold a mutable value. Two reads of one element share it.
// =============================================================================

// ===== SHOULD REPORT =====

// crMapValue: the value read from the map is branched twice.
func crMapValue(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	m := map[string]*gorm.DB{"adult": base.Where("age > ?", 18)}
	q := m["adult"]
	q.Count(nil)
	q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crMapValueCommaOk: the same through a comma-ok read.
func crMapValueCommaOk(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	m := map[string]*gorm.DB{"adult": base.Where("age > ?", 18)}
	if q, ok := m["adult"]; ok {
		q.Count(nil)
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// crMapReadTwice: two reads of one key read one Statement.
func crMapReadTwice(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	m := map[string]*gorm.DB{"adult": base.Where("age > ?", 18)}
	m["adult"].Count(nil)
	m["adult"].Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crRangeSlice: each element is branched twice.
func crRangeSlice(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	qs := []*gorm.DB{base.Where("a"), base.Where("b")}
	for _, q := range qs {
		q.Where("x").Find(nil)
		q.Where("y").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// crRangeMap: the same for the values of a map.
func crRangeMap(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	m := map[string]*gorm.DB{"a": base.Where("a")}
	for _, q := range m {
		q.Count(nil)
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// crSliceIndexTwice: two reads of s[0].
func crSliceIndexTwice(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	s := []*gorm.DB{base.Where("a")}
	s[0].Find(nil)
	s[0].Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crArrayIndexTwice: the same for an array.
func crArrayIndexTwice(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	a := [1]*gorm.DB{base.Where("a")}
	a[0].Find(nil)
	a[0].Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crAppended: append fills the slice.
func crAppended(db *gorm.DB, items []string) {
	base := db.Session(&gorm.Session{})
	var qs []*gorm.DB
	for _, item := range items {
		qs = append(qs, base.Where("name = ?", item))
	}
	for _, q := range qs {
		q.Count(nil)
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// crChannelReceive: the value received is branched twice.
func crChannelReceive(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	ch := make(chan *gorm.DB, 1)
	ch <- base.Where("age > ?", 18)
	q := <-ch
	q.Where("a").Find(nil)
	q.Where("b").Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crRangeChannelParam: a channel parameter may carry any value.
func crRangeChannelParam(ch chan *gorm.DB) {
	for q := range ch {
		q.Count(nil)
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// crSliceParam: so may a slice parameter.
func crSliceParam(qs []*gorm.DB) {
	for _, q := range qs {
		q.Count(nil)
		q.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
	}
}

// crPointerParam: two reads through a pointer parameter read one Statement.
func crPointerParam(pq **gorm.DB) {
	(*pq).Find(nil)
	(*pq).Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

// crFilledByHelper: a helper that may reach gorm may fill the slice.
func crFilledByHelper(db *gorm.DB) {
	qs := make([]*gorm.DB, 1)
	crFill(db, qs)
	qs[0].Find(nil)
	qs[0].Count(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}

func crFill(db *gorm.DB, qs []*gorm.DB) { qs[0] = db.Session(&gorm.Session{}) }

// ===== SHOULD NOT REPORT =====

// crImmutableValues: the map holds only immutable values.
func crImmutableValues(db *gorm.DB) {
	m := map[string]*gorm.DB{"all": db.Session(&gorm.Session{})}
	q := m["all"]
	q.Count(nil)
	q.Find(nil)
}

// crRangeImmutable: the same for a slice.
func crRangeImmutable(db *gorm.DB) {
	qs := []*gorm.DB{db.Session(&gorm.Session{}), db.WithContext(nil)}
	for _, q := range qs {
		q.Count(nil)
		q.Find(nil)
	}
}

// crEachElementOnce: each element is branched once.
func crEachElementOnce(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	qs := []*gorm.DB{base.Where("a"), base.Where("b")}
	for _, q := range qs {
		q.Find(nil)
	}
	qs[0].Count(nil)
}

// crTwoElements: s[0] and s[1] are two elements.
func crTwoElements(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	s := []*gorm.DB{base.Where("a"), base.Where("b")}
	s[0].Find(nil)
	s[1].Find(nil)
}

// crRebuiltInPlace: the element is written between the reads, so each read
// is a root of its own.
func crRebuiltInPlace(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	s := []*gorm.DB{base.Where("a")}
	s[0] = s[0].Where("b")
	s[0].Find(nil)
}

// crPointerParamRebuilt: the same through a pointer parameter.
func crPointerParamRebuilt(pq **gorm.DB) {
	*pq = (*pq).Where("tenant = ?", 1)
	(*pq).Find(nil)
}

// crTwoReceives: each receive reads a value of its own.
func crTwoReceives(db *gorm.DB) {
	base := db.Session(&gorm.Session{})
	ch := make(chan *gorm.DB, 2)
	ch <- base.Where("a")
	ch <- base.Where("b")
	(<-ch).Find(nil)
	(<-ch).Find(nil)
}

// crSortedImmutable: a function that cannot reach gorm puts no *gorm.DB in
// the slice.
func crSortedImmutable(db *gorm.DB) {
	qs := []*gorm.DB{db.Session(&gorm.Session{})}
	slices.Reverse(qs)
	qs[0].Find(nil)
	qs[0].Count(nil)
}

type crRepo struct{ qs []*gorm.DB }

// crFieldContainer is missed: a container read from a field is not followed,
// as a field read through a receiver is not. [LIMITATION]
func (r *crRepo) crFieldContainer() {
	r.qs[0].Find(nil)
	r.qs[0].Count(nil)
}
