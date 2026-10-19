package fix

import (
	"cmp"
	"go/ast"
	"go/token"
	"slices"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/cfg"
	"github.com/mpyw/gormreuse/internal/ssa/pollution"
	"github.com/mpyw/gormreuse/internal/typeutil"
)

// sessionSite is a place where a fix writes a Session: prefix, then
// ".Session(&gorm.Session{})", then suffix, inserted at pos. A Session
// appended to a call has neither; a parameter's has a whole statement around
// it (see paramSessionSite).
//
//declscope:shared // generator.go and param.go build the sites
type sessionSite struct {
	pos            token.Pos
	prefix, suffix string
}

// sessionEdits writes a Session at each of sites. It qualifies Session with a
// name that refers to gorm.io/gorm at every one of them, and adds an import of
// gorm.io/gorm to a file where no such name exists (see gormNameAt).
//
//declscope:shared // generator.go's Generate and GenerateIsolation write the Sessions with it
func (g *Generator) sessionEdits(sites []sessionSite) []analysis.TextEdit {
	byFile := make(map[*ast.File][]sessionSite)
	var files []*ast.File
	for _, site := range sites {
		file := g.findFileContaining(site.pos)
		if _, seen := byFile[file]; !seen {
			files = append(files, file)
		}
		byFile[file] = append(byFile[file], site)
	}
	var edits []analysis.TextEdit
	for _, file := range files {
		var positions []token.Pos
		for _, site := range byFile[file] {
			positions = append(positions, site.pos)
		}
		qualifier, importName := g.gormNameAt(file, positions)
		for _, site := range byFile[file] {
			edits = append(edits, analysis.TextEdit{
				Pos:     site.pos,
				End:     site.pos,
				NewText: []byte(site.prefix + ".Session(&" + qualifier + "Session{})" + site.suffix),
			})
		}
		if importName != "" {
			edits = append(edits, gormImportEdit(file, importName))
		}
	}
	return edits
}

// findRootsNeedingSession finds the roots — the original AND reassignment-created
// virtual roots — that still branch 2+ times on one path after reassignment
// simulation and therefore need a Session() to become immutable.
//
// Emitting Session for virtual roots too is what makes a fix single-pass
// complete (#71 defect 2): for `q := db.Where("base"); q.Where("a");
// q.Where("b").Find(); q.Where("c"); q.Where("d").Find()` the reassignment of
// the virtual root created at `q.Where("a")` still branches twice, so it needs
// `.Session()` — without it, re-linting the "fixed" output still warns.
//
// Two uses of a root branch it when the earlier one can reach the later one
// (see usesConflict). A reassignment does not branch the root it reads with
// the uses after it: on a path through the reassignment those uses see the
// new root, so `for ... { q = q.Where(x) }; q.Find(nil)` needs no Session on
// the original root.
//
// The simulation orders uses by position, so it misses the uses that a loop's
// next iteration makes of a virtual root created in the loop body. Such a root
// always gets a Session (#199): in `for { q.Where("id").Find(nil); q = q.Where("age") }`
// the next iteration's q.Where("id") would otherwise change it in place.
//
// The path rules hold only when ordered is set, that is, when the uses are
// every use of the root, in the function that defines it (see usesOrdered).
// Otherwise a root gets a Session when it has a use other than the one
// reassignment that reads it.
//
//declscope:shared // generator.go's Generate asks it which roots get a Session
func (g *Generator) findRootsNeedingSession(virtualUses map[virtualRootKey][]pollution.UsageInfo, reassigned map[token.Pos]token.Pos, ordered bool) []virtualRootKey {
	var roots []virtualRootKey
	cfgAnalyzer := cfg.NewAnalyzer()
	var def *ssa.BasicBlock
	for root := range virtualUses {
		if root.isOriginal {
			def = root.block
		}
	}
	for root, uses := range virtualUses {
		var needs bool
		switch {
		case !root.isOriginal && useRepeatsAfter(root.block, def):
			needs = true
		case !ordered:
			needs = len(uses) >= 2 || len(uses) == 1 && (!root.isOriginal || !reassigned[uses[0].Pos].IsValid())
		case root.isOriginal && usedAgainInLoop(root.block, uses, reassigned):
			needs = true
		default:
			needs = usesConflict(cfgAnalyzer, uses, reassigned)
		}
		if needs {
			roots = append(roots, root)
		}
	}
	// Deterministic order (map iteration is random) so edits/goldens are stable.
	slices.SortFunc(roots, func(a, b virtualRootKey) int { return cmp.Compare(a.pos, b.pos) })
	return roots
}

// sessionPositionsForRoot returns where Sessions go to make root immutable.
// For a Phi node it returns a position for each incoming edge that can take
// one. Otherwise it returns the position after the call at pos and those of
// the other values its uses may read instead of it (see mergedEdges).
//
// The root's own Session is needed even when the Phi it flows into is used
// once: the root has 2+ uses, and some of them may come before the merge, as
// in `q := db.Where("x"); q.Find(nil); q.Count(nil); if c { q = db.Where("z") }`
// (#202).
//
//declscope:shared // generator.go's Generate places the original root's Sessions with it
func (g *Generator) sessionPositionsForRoot(pos token.Pos, root ssa.Value, uses []pollution.UsageInfo) []token.Pos {
	if phi, isPhi := root.(*ssa.Phi); isPhi {
		if !mergeUsedTwice(phi) {
			return nil
		}
		return g.edgeSessionPositions(phi.Edges)
	}

	var positions []token.Pos
	if end := g.sessionEndAfterCall(pos); end.IsValid() {
		positions = append(positions, end)
	}
	return append(positions, g.edgeSessionPositions(mergedEdges(root, uses))...)
}

// edgeSessionPositions returns where Sessions go on edges, the values that
// merge with a root. Only edges that don't already have Session get one, and
// only an edge defined by a *gorm.DB call can (see sessionEndAfterCall).
func (g *Generator) edgeSessionPositions(edges []ssa.Value) []token.Pos {
	var positions []token.Pos
	for _, edge := range edges {
		// Skip nil constants
		if c, ok := edge.(*ssa.Const); ok && c.Value == nil {
			continue
		}

		// Skip an edge that is already immutable by the tracer's rule: its own
		// call, the last of its chain, returns an immutable value. A Session
		// earlier in the chain, as in db.Session(s).Where("y"), does not count,
		// and the edge would stay reported after every pass (#201).
		if g.tracer.IsImmutableCall(edge) {
			continue
		}

		if end := g.sessionEndAfterCall(edge.Pos()); end.IsValid() {
			positions = append(positions, end)
		}
	}
	return positions
}

// sessionEndAfterCall returns where a Session goes for a value defined at
// pos: the end of the call whose Lparen is pos, where SSA places a call. It
// returns token.NoPos when there is no such call or the call does not return
// *gorm.DB. A closure parameter or the result of a call with several results
// has no call of its own, and appending a Session to the call that encloses
// it, such as `run(db, func(tx *gorm.DB) { ... })`, would not compile (#200).
func (g *Generator) sessionEndAfterCall(pos token.Pos) token.Pos {
	file := g.findFileContaining(pos)
	if file == nil {
		return token.NoPos
	}
	call := findCallAtLparen(g.fileCursor(file), pos)
	if call == nil || !typeutil.IsGormDB(g.pass.TypesInfo.TypeOf(call)) {
		return token.NoPos
	}
	return call.End()
}
