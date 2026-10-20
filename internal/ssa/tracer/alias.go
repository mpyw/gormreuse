package tracer

import (
	"math"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/callsite"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollutionsource"
)

// FindAliasedRoots returns the mutable roots whose Statement v may read later:
// through a Session or Debug copy (#174), or as a read-only argument of a call
// in v's chain, such as a subquery (#185).
//
// On a clone==0 receiver, Session(&gorm.Session{}) and Debug() return a handle
// that shares the receiver's Statement. Each use of the copy forks its own
// Statement, so it does not change the receiver. But it reads the receiver's
// Statement as it is at that moment. So each use of the copy is a pure use of
// the receiver's root at the use's own position:
//
//	q := db.Where("tenant")       // root
//	countQ := q.Session(&gorm.Session{})
//	q.Where("active").Find(nil)   // changes q's Statement in place
//	countQ.Count(nil)             // reads it: pure use of q's root, after a branch
//
// Copies of copies are followed, so q.Session(...).Debug() still reaches q's
// root. A copy reached through a Phi, a variable, or a closure capture is found
// the same way as a mutable root.
//
// A subquery is kept in the Statement of the chain it is passed to, and GORM
// builds it from the argument when the chain runs. So each use of the chain
// reads the argument's root too:
//
//	x := db.Where("id IN (?)", sub)
//	sub.Where("age > 1").Find(nil) // changes sub's Statement in place
//	x.Find(nil)                    // builds the subquery from it
//
// Every call in v's chain is searched for such arguments, through Session and
// WithContext too, since a cloned Statement still holds the argument. A
// Session copy below another chain call is not an alias: that call forked its
// own Statement.
func (t *RootTracer) FindAliasedRoots(v ssa.Value, loopInfo *cfg.LoopInfo) []AliasedRoot {
	var roots []AliasedRoot
	t.collectAliasedRoots(v, rootsLoopInfo(loopInfo), true, make(map[aliasedRootVisit]bool), &roots)
	return roots
}

// AliasedRoot is a mutable root that a value reads, and the call it reads it
// through: the Session or Debug call that took the copy, or, when Argument is
// set, the call that took the root as a read-only argument.
type AliasedRoot struct {
	Root     ssa.Value
	Copy     *ssa.Call
	Argument bool
}

// aliasedRootVisit is a call visited by collectAliasedRoots, and whether Session
// and Debug copies still counted there.
type aliasedRootVisit struct {
	call   *ssa.Call
	copies bool
}

// collectAliasedRoots adds to roots the mutable roots that v reaches through
// Statement-sharing builtin calls, while copies is set, and through read-only
// arguments of the gorm calls in its chain. seen stops cycles through loops.
//
// A search that mayAlias says adds nothing is skipped. Without that, a chain
// of n conditional reassignments takes time cubic in n (#260): each of the n
// calls searches n steps down the chain, and each step goes through the n
// roots of its receiver. Skipping such a search does not change what is added
// or in what order: everything it would reach adds nothing either.
func (t *RootTracer) collectAliasedRoots(v ssa.Value, loopInfo *cfg.LoopInfo, copies bool, seen map[aliasedRootVisit]bool, roots *[]AliasedRoot) {
	if !t.mayAlias(v, loopInfo, copies) {
		return
	}
	for _, candidate := range t.FindAllMutableRoots(v, loopInfo) {
		call, site, ok := t.aliasStep(candidate)
		if !ok || seen[aliasedRootVisit{call, copies}] {
			continue
		}
		seen[aliasedRootVisit{call, copies}] = true
		isCopy := copies && t.aliasSharesStatement(site)
		if isCopy {
			if r := t.FindMutableRoot(site.Recv, loopInfo); r != nil {
				*roots = append(*roots, AliasedRoot{Root: r, Copy: call})
			}
		}
		for _, arg := range pollutionsource.ReadOnlyGormArgs(site) {
			if r := t.FindMutableRoot(arg, loopInfo); r != nil {
				*roots = append(*roots, AliasedRoot{Root: r, Copy: call, Argument: true})
			}
			t.collectAliasedRoots(arg, loopInfo, true, seen, roots)
		}
		t.collectAliasedRoots(site.Recv, loopInfo, isCopy, seen, roots)
	}
}

// mayAlias reports whether collectAliasedRoots(v, loopInfo, copies, ...) may
// add a root. The answer is computed once per search.
//
// The searches form a graph: a search goes on to the receiver and the
// read-only arguments of each call it steps through. A search may add a root
// when it, or a search it reaches, adds one at its own step: the least fixed
// point. A loop makes a cycle in this graph, and every search on a cycle
// reaches every other, so they all get the same answer. mayAliasVisit finds
// the cycles with Tarjan's algorithm and answers each one when it is complete,
// so a cycle that adds nothing is answered false and skipped too (#300). Each
// search is visited once, as before.
func (t *RootTracer) mayAlias(v ssa.Value, loopInfo *cfg.LoopInfo, copies bool) bool {
	may, _ := t.mayAliasVisit(aliasedRootsKey{v, loopInfo, copies}, &aliasCycles{index: make(map[aliasedRootsKey]int)})
	return may
}

// aliasCycles is the state of one mayAlias computation: the searches on its
// stack, in the order mayAliasVisit reached them, whose cycle is not complete
// yet.
type aliasCycles struct {
	index map[aliasedRootsKey]int // Position of each search on the stack; an answered one is in RootTracer.aliases
	stack []aliasedRootsKey
	may   []bool // Whether each search on the stack adds a root itself or through what it has visited
}

// mayAliasVisit answers mayAlias for key, and returns the lowest stack
// position key reaches, or math.MaxInt once key is answered. A search still
// on the stack answers what it knows so far: false, unless it has found a
// root. The first search of a cycle answers for all of it when it is done.
func (t *RootTracer) mayAliasVisit(key aliasedRootsKey, s *aliasCycles) (bool, int) {
	if may, ok := t.aliases[key]; ok {
		return may, math.MaxInt
	}
	if i, ok := s.index[key]; ok {
		return s.may[i], i
	}
	at := len(s.stack)
	s.index[key] = at
	s.stack = append(s.stack, key)
	s.may = append(s.may, false)
	low := at
	visit := func(next aliasedRootsKey) bool {
		may, reached := t.mayAliasVisit(next, s)
		low = min(low, reached)
		return may
	}
	may := slices.ContainsFunc(t.FindAllMutableRoots(key.v, key.loopInfo), func(candidate ssa.Value) bool {
		_, site, ok := t.aliasStep(candidate)
		if !ok {
			return false
		}
		isCopy := key.copies && t.aliasSharesStatement(site)
		if isCopy && t.FindMutableRoot(site.Recv, key.loopInfo) != nil {
			return true
		}
		for _, arg := range pollutionsource.ReadOnlyGormArgs(site) {
			if t.FindMutableRoot(arg, key.loopInfo) != nil || visit(aliasedRootsKey{arg, key.loopInfo, true}) {
				return true
			}
		}
		return visit(aliasedRootsKey{site.Recv, key.loopInfo, isCopy})
	})
	s.may[at] = may
	if low < at {
		return may, low
	}
	// key is the first search of its cycle on the stack, and the cycle is
	// complete: the searches above it are the rest of it.
	may = slices.Contains(s.may[at:], true)
	for _, member := range s.stack[at:] {
		t.aliases[member] = may
	}
	s.stack, s.may = s.stack[:at], s.may[:at]
	return may, math.MaxInt
}

// aliasStep returns the gorm method call that collectAliasedRoots goes
// through at candidate, and its Site, or false when it does not go through
// candidate. A call through a method value (w := q.Where; w(x, sub)) is read
// as the method call it stands for (#178).
func (t *RootTracer) aliasStep(candidate ssa.Value) (*ssa.Call, callsite.Site, bool) {
	call, ok := candidate.(*ssa.Call)
	if !ok {
		return nil, callsite.Site{}, false
	}
	site := t.Site(call)
	if !site.IsGormMethod() || site.Recv == nil {
		return nil, callsite.Site{}, false
	}
	return call, site, true
}

// aliasSharesStatement reports whether site is a gorm Session or Debug
// method call whose result may share its receiver's Statement.
func (t *RootTracer) aliasSharesStatement(site callsite.Site) bool {
	if !t.isImmutableReturningBuiltin(site.Declared) || site.Recv == nil {
		return false
	}
	return site.SessionEffect().SharesStatement
}
