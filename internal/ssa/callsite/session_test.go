package callsite

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// sessionSource stands in for gorm: readSessionLiteral reads any struct
// literal passed to a method named Session.
const sessionSource = `package p

type Session struct {
	NewDB       bool
	Initialized bool
}

type DB struct{}

func (db *DB) Session(s *Session) *DB { return db }
func (db *DB) Debug() *DB             { return db }
func (db *DB) Where() *DB             { return db }

func other(s *Session) {}

func param(db *DB, s *Session) { db.Session(s) }

func initialized(db *DB) { db.Session(&Session{Initialized: true}) }

func passedElsewhere(db *DB) {
	s := &Session{}
	other(s)
	db.Session(s)
}

func readField(db *DB) bool {
	s := &Session{}
	db.Session(s)
	return s.NewDB
}

func storedTwice(db *DB) {
	s := &Session{NewDB: true}
	s.NewDB = false
	db.Session(s)
}

func storedLater(db *DB, c bool) {
	s := &Session{}
	if c {
		s.Initialized = true
	}
	db.Session(s)
}

func storedValue(db *DB, v bool) { db.Session(&Session{Initialized: v}) }

func debug(db *DB) { db.Debug() }

func where(db *DB) { db.Where() }

func boxed(db *DB) any {
	s := &Session{}
	db.Session(s)
	return s
}

func newInt() *int { return new(int) }
`

// buildSessionPackage builds sessionSource into SSA.
func buildSessionPackage(t *testing.T) *ssa.Package {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", sessionSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset,
		types.NewPackage("p", "p"), []*ast.File{file}, ssa.SanityCheckFunctions)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

// sessionCallOf returns the Site of the last call in function name.
func sessionCallOf(t *testing.T, pkg *ssa.Package, name string) Site {
	t.Helper()
	var last ssa.CallInstruction
	for _, block := range pkg.Func(name).Blocks {
		for _, instr := range block.Instrs {
			if call, ok := instr.(*ssa.Call); ok {
				last = call
			}
		}
	}
	if last == nil {
		t.Fatalf("no call in %s", name)
	}
	return SiteOf(last, FuncVars{})
}

func TestSessionEffect(t *testing.T) {
	t.Parallel()
	pkg := buildSessionPackage(t)
	unknown := SessionEffect{SharesStatement: true, ReturnsMutable: true}
	tests := []struct {
		fn          string
		want        SessionEffect
		initialized bool
	}{
		{fn: "param", want: unknown},
		{fn: "initialized", want: SessionEffect{ReturnsMutable: true}, initialized: true},
		{fn: "passedElsewhere", want: unknown},
		{fn: "storedTwice", want: SessionEffect{SharesStatement: true}},
		{fn: "storedLater", want: unknown},
		{fn: "storedValue", want: unknown},
		{fn: "debug", want: SessionEffect{SharesStatement: true}},
		{fn: "where", want: SessionEffect{}},
		{fn: "boxed", want: unknown},
	}
	for _, tt := range tests {
		site := sessionCallOf(t, pkg, tt.fn)
		if got := site.SessionEffect(); got != tt.want {
			t.Errorf("%s: SessionEffect() = %+v, want %+v", tt.fn, got, tt.want)
		}
		if got := site.SessionSetsInitialized(); got != tt.initialized {
			t.Errorf("%s: SessionSetsInitialized() = %v, want %v", tt.fn, got, tt.initialized)
		}
	}
}

func TestSessionLiteralOfOtherTypes(t *testing.T) {
	t.Parallel()
	pkg := buildSessionPackage(t)

	// A Session method whose config is read elsewhere is not a literal.
	if _, ok := readSessionLiteral(sessionCallOf(t, pkg, "readField").sessionConfig()); ok {
		t.Error("a literal whose field is read must not be readable")
	}
	// new(int) is an Alloc of a non-struct.
	var alloc *ssa.Alloc
	for _, instr := range pkg.Func("newInt").Blocks[0].Instrs {
		if a, ok := instr.(*ssa.Alloc); ok {
			alloc = a
		}
	}
	if _, ok := readSessionLiteral(alloc); ok {
		t.Error("an Alloc of an int must not be readable")
	}
	// A Site with no arguments and no callee.
	if (Site{}).sessionConfig() != nil || (Site{}).Method() != "" {
		t.Error("a zero Site must have no config and no method")
	}
}
