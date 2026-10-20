package fix

import (
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/effort"
	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

// virtualRootKey represents a virtual root (either original or created by
// reassignment).
//
//declscope:shared // generator.go and session.go read the virtual roots
type virtualRootKey struct {
	isOriginal bool
	pos        token.Pos       // For virtual roots, the position where created
	end        token.Pos       // For virtual roots, the end of the reassigned chain
	block      *ssa.BasicBlock // The block of the reassignment, or of the original root's definition (nil if unknown)
}

// simulateVirtualRoots simulates Phase 1 reassignments and returns the virtual
// roots they make, as a map from virtual root keys to their uses.
//
// A use is a use of every root it may see: each earlier reassignment that can
// reach it, back to the first one whose block dominates it. When none
// dominates it, it may also see the original root. So in
// `if c { q.Where("a") }; q.Where("b").Find(nil); q.Where("c").Find(nil)` the
// original root, used on the path where c is false, needs a Session as well
// as the reassignment in the if (#202). Uses are taken in position order, the
// uses of one block in run order (see runOrderedUses), so a loop's next
// iteration is not seen here (see findRootsNeedingSession).
//
//declscope:shared // generator.go's Generate runs the simulation with it
func (g *Generator) simulateVirtualRoots(root ssa.Value, uses []pollution.UsageInfo, reassigned map[token.Pos]token.Pos) map[virtualRootKey][]pollution.UsageInfo {
	sortedUses := runOrderedUses(uses)

	original := virtualRootKey{isOriginal: true, pos: root.Pos(), block: tracer.RootBlock(root)}
	virtualUses := make(map[virtualRootKey][]pollution.UsageInfo)

	// The virtual roots made so far, in position order.
	var made []virtualRootKey
	for _, use := range sortedUses {
		dominated := false
		for _, vr := range slices.Backward(made) {
			reaches, dominates := virtualRootReaches(g.cfg, vr.block, use.Block)
			if reaches {
				virtualUses[vr] = append(virtualUses[vr], use)
			}
			if dominates {
				dominated = true
				break
			}
		}
		if !dominated {
			virtualUses[original] = append(virtualUses[original], use)
		}

		// If non-finisher, this use's result becomes the new root
		if end, ok := reassigned[use.Pos]; ok {
			// Create a new virtual root at this position. Its Session goes
			// after the chain's last call: one after an earlier call would be
			// undone by the calls that follow it.
			vr := virtualRootKey{isOriginal: false, pos: use.Pos, end: end, block: use.Block}
			made = append(made, vr)
			// A root with no later use still needs a Session when a loop
			// runs it again (see findRootsNeedingSession).
			virtualUses[vr] = nil
		}
	}

	return virtualUses
}

// virtualRootReaches reports whether a use in block use, later in position,
// can see the virtual root of a reassignment made in block def, and whether
// every path to it passes the reassignment, so that it cannot see any earlier
// root. Blocks of different functions, such as a use inside a closure called
// inline, are taken in position order, so the reassignment both reaches and
// dominates the use.
func virtualRootReaches(a *cfg.Analyzer, def, use *ssa.BasicBlock) (reaches, dominates bool) {
	if def == use || def.Parent() != use.Parent() {
		return true, true
	}
	return a.CanReach(def, use), def.Dominates(use)
}

// virtualRootEndsImmutable reports whether the chain that vr, a virtual root,
// reassigns already ends in a call whose result is immutable by the tracer's
// rule, as q.Where("a").Session(&gorm.Session{}) does. A second Session after
// it would be redundant (#202). The call that ends the chain, and its SSA
// call, are looked up in indexes of the file and of the function, not found
// by a walk of them per virtual root (#377).
//
//declscope:shared // generator.go's Generate skips a redundant Session with it
func (g *Generator) virtualRootEndsImmutable(vr virtualRootKey) bool {
	// A statement in parentheses, (q.Where("a")), ends after its call, and
	// leaves lparen invalid.
	lparen := g.callEndingAt(g.findFileContaining(vr.end), vr.end)
	if !lparen.IsValid() {
		return false
	}
	fn := vr.block.Parent()
	calls, ok := g.ssaCalls[fn]
	if !ok {
		calls = make(map[token.Pos]*ssa.Call)
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				effort.Step()
				if call, ok := instr.(*ssa.Call); ok {
					if _, seen := calls[call.Pos()]; !seen {
						calls[call.Pos()] = call
					}
				}
			}
		}
		if g.ssaCalls == nil {
			g.ssaCalls = make(map[*ssa.Function]map[token.Pos]*ssa.Call)
		}
		g.ssaCalls[fn] = calls
	}
	if call, ok := calls[lparen]; ok {
		return g.tracer.IsImmutableCall(call)
	}
	return false
}
