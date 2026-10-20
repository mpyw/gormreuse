package tracer

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/effort"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// unclonedStateMethods are the gorm methods that set Statement state which
// Statement.clone() does not copy (#344). The next call on a Session result
// clones the Statement, so a Session after one of them drops what it set:
//   - Attrs and Assign set Statement.attrs and Statement.assigns, which
//     FirstOrInit and FirstOrCreate read.
//   - InstanceSet stores into Statement.Settings under a key that holds the
//     Statement's address, so the clone's InstanceGet does not find it.
//
// e2e/internal/uncloned_state_test.go checks each Statement field that
// clone() leaves out against the real gorm, and lists the others.
var unclonedStateMethods = map[string]bool{
	"Attrs":       true,
	"Assign":      true,
	"InstanceSet": true,
}

// unclonedStateReaders are the finishers that read Statement.attrs and
// Statement.assigns.
var unclonedStateReaders = map[string]bool{
	"FirstOrInit":   true,
	"FirstOrCreate": true,
}

// SessionDropsState reports whether a Session that made v immutable may drop
// state that v's Statement carries, which Statement.clone() does not copy
// (see unclonedStateMethods). That holds when:
//   - v's own chain, back to a Session-family call or the value it starts
//     from, calls Attrs, Assign or InstanceSet;
//   - a chain on v calls one, and v keeps its effect: the chain runs no
//     query, as the discarded statement `q.Assign(x)`, or its value merges
//     back, as in `if c { q = q.Assign(x) }`, or goes anywhere else;
//   - v is a parameter, which may carry the caller's Attrs or Assign, and a
//     chain on it reaches FirstOrInit or FirstOrCreate, the finishers that
//     read them.
//
// An immutable value never carries such state: the next call on it clones
// its Statement. The suggested fixes withhold every Session for such a value,
// and its diagnostics do not advise one (#344).
//
// The answer is memoized per value. The fix asks it of every value merged
// with each root, and each walk follows the whole Phi chain, so asking again
// made a use in a loop after n filters cubic (#377).
func (t *RootTracer) SessionDropsState(v ssa.Value) bool {
	if drops, ok := t.sessionDrops[v]; ok {
		return drops
	}
	drops := t.sessionDropsState(v)
	t.sessionDrops[v] = drops
	return drops
}

// sessionDropsState is SessionDropsState without the memo.
func (t *RootTracer) sessionDropsState(v ssa.Value) bool {
	if t.chainSetsState(v, make(map[ssa.Value]bool)) {
		return true
	}
	kept, reads := t.stateUsesFrom(v, false, make(map[ssa.Value]bool))
	_, isParam := v.(*ssa.Parameter)
	return kept || isParam && reads
}

// chainSetsState reports whether v's chain, walked back through *gorm.DB
// methods and Phis, calls an uncloned-state method.
func (t *RootTracer) chainSetsState(v ssa.Value, visited map[ssa.Value]bool) bool {
	if visited[v] {
		return false
	}
	visited[v] = true
	effort.Step()
	switch v := v.(type) {
	case *ssa.Call:
		site := t.Site(v)
		if !site.IsGormMethod() || site.Recv == nil {
			return false
		}
		switch method := site.Method(); {
		case unclonedStateMethods[method]:
			return true
		case typeutil.IsImmutableReturningBuiltin(method):
			// The next call on the result clones the Statement.
			return false
		}
		// A finisher on a clone==0 value returns that value, so its chain
		// is followed as a chain method's is.
		return t.chainSetsState(site.Recv, visited)
	case *ssa.Phi:
		for _, edge := range v.Edges {
			if t.chainSetsState(edge, visited) {
				return true
			}
		}
	}
	return false
}

// stateUsesFrom walks the chains on v forward through *gorm.DB methods and
// Phis. sets is set when the chain from the start of the walk to v called an
// uncloned-state method. kept reports such a chain whose value is not
// consumed by a finisher or a Session-family call: nothing reads it, or it
// goes anywhere other than a *gorm.DB method's receiver. reads reports a
// chain that reaches FirstOrInit or FirstOrCreate.
func (t *RootTracer) stateUsesFrom(v ssa.Value, sets bool, visited map[ssa.Value]bool) (kept, reads bool) {
	if visited[v] {
		return false, false
	}
	visited[v] = true
	effort.Step()
	refs := v.Referrers()
	if refs == nil {
		// A value with no referrer list, such as a FieldRoot, is not
		// followed.
		return false, false
	}
	used := len(*refs) > 0
	for _, ref := range *refs {
		var next ssa.Value
		nextSets := sets
		switch ref := ref.(type) {
		case *ssa.Phi:
			next = ref
		case *ssa.Call:
			site := t.Site(ref)
			if !site.IsGormMethod() || site.Recv != v {
				kept = kept || sets
				continue
			}
			method := site.Method()
			reads = reads || unclonedStateReaders[method]
			if !typeutil.IsGormDB(ref.Type()) || typeutil.IsFinisher(method) || typeutil.IsImmutableReturningBuiltin(method) {
				continue
			}
			next, nextSets = ref, sets || unclonedStateMethods[method]
		default:
			kept = kept || sets
			continue
		}
		if _, isPhi := next.(*ssa.Phi); isPhi && sets {
			kept = true
		}
		k, r := t.stateUsesFrom(next, nextSets, visited)
		kept, reads = kept || k, reads || r
	}
	return kept || sets && !used, reads
}
