package tracer

import (
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
	t.collectAliasedRoots(v, loopInfo, true, make(map[aliasedRootVisit]bool), &roots)
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
// add a root. The answer is computed once per search. A search that reaches
// itself again through a loop is assumed to add one.
func (t *RootTracer) mayAlias(v ssa.Value, loopInfo *cfg.LoopInfo, copies bool) bool {
	key := aliasedRootsKey{v, loopInfo, copies}
	if may, ok := t.aliases[key]; ok {
		return may
	}
	t.aliases[key] = true
	may := slices.ContainsFunc(t.FindAllMutableRoots(v, loopInfo), func(candidate ssa.Value) bool {
		_, site, ok := t.aliasStep(candidate)
		if !ok {
			return false
		}
		isCopy := copies && t.aliasSharesStatement(site)
		if isCopy && t.FindMutableRoot(site.Recv, loopInfo) != nil {
			return true
		}
		for _, arg := range pollutionsource.ReadOnlyGormArgs(site) {
			if t.FindMutableRoot(arg, loopInfo) != nil || t.mayAlias(arg, loopInfo, true) {
				return true
			}
		}
		return t.mayAlias(site.Recv, loopInfo, isCopy)
	})
	t.aliases[key] = may
	return may
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
