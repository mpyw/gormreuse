package directive

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// parseFileWithTypes parses src as a single Go file in package "demo" and
// runs go/types over it so the resulting *ast.File / *types.Info combo can
// drive ImmutableInputSet.AddFile in tests.
func parseFileWithTypes(t *testing.T, src string) (*token.FileSet, *ast.File, *types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "demo.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := &types.Config{Importer: importer.Default()}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	if _, err := conf.Check("demo", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type check: %v", err)
	}
	return fset, file, info
}

func TestImmutableInputSet_NilSafety(t *testing.T) {
	t.Parallel()

	var nilSet *ImmutableInputSet
	// Every accessor must tolerate a nil receiver — the analyzer routinely
	// passes nil sets when the directive bookkeeping isn't applicable.
	nilSet.AddFile(nil, "demo")
	if got := nilSet.AllCallbacks(nil); got != nil {
		t.Errorf("AllCallbacks(nil) = %v, want nil", got)
	}
	if got := nilSet.GetUnused(); got != nil {
		t.Errorf("GetUnused() = %v, want nil", got)
	}
}

func TestImmutableInputSet_AddFile_NoDirectives(t *testing.T) {
	t.Parallel()

	const src = `package demo

func plain(cb func()) {}
`
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	if got := len(s.GetUnused()); got != 0 {
		t.Errorf("GetUnused() with no directives = %d, want 0", got)
	}
}

func TestImmutableInputSet_AddFile_ParamNotFound(t *testing.T) {
	t.Parallel()

	const src = `package demo

//gormreuse:immutable-input(missing)
func noSuchParam(cb func()) {}
`
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	unused := s.GetUnused()
	if len(unused) != 1 {
		t.Fatalf("len(GetUnused()) = %d, want 1; entries=%+v", len(unused), unused)
	}
	if got := unused[0].Reason; !contains(got, "parameter \"missing\" not found") {
		t.Errorf("Reason = %q, want substring `parameter \"missing\" not found`", got)
	}
}

func TestImmutableInputSet_AddFile_ParamNotFunctionType(t *testing.T) {
	t.Parallel()

	const src = `package demo

//gormreuse:immutable-input(x)
func notFn(x int) {}
`
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	unused := s.GetUnused()
	if len(unused) != 1 {
		t.Fatalf("len(GetUnused()) = %d, want 1", len(unused))
	}
	if got := unused[0].Reason; !contains(got, `parameter "x" is not a function type`) {
		t.Errorf("Reason = %q, want `not a function type`", got)
	}
}

// TestImmutableInputSet_AddFile_CallbackHasNoGormDB exercises the U3 branch:
// the named parameter is a function type, but its signature has no *gorm.DB
// argument, so the directive can never apply.
func TestImmutableInputSet_AddFile_CallbackHasNoGormDB(t *testing.T) {
	t.Parallel()

	const src = `package demo

//gormreuse:immutable-input(cb)
func cbNoGormDB(cb func(int) int) {}
`
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	unused := s.GetUnused()
	if len(unused) != 1 {
		t.Fatalf("len(GetUnused()) = %d, want 1", len(unused))
	}
	if got := unused[0].Reason; !contains(got, "no *gorm.DB parameter") {
		t.Errorf("Reason = %q, want `no *gorm.DB parameter`", got)
	}
}

// TestImmutableInputSet_AddFile_AnonymousParam covers the lookupParam branch
// where a field has no Names slice — that field still consumes a position
// but cannot match any name. Ensures the index is advanced correctly so the
// next named parameter resolves.
func TestImmutableInputSet_AddFile_AnonymousParam(t *testing.T) {
	t.Parallel()

	// First parameter is anonymous (`int`), second is the directive target.
	// We expect U1 (parameter not found) for an unrelated name to confirm
	// lookupParam walked past the anonymous slot without crashing.
	const src = `package demo

//gormreuse:immutable-input(missing)
func anon(int, cb func()) {}
`
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	unused := s.GetUnused()
	if len(unused) != 1 {
		t.Fatalf("len(GetUnused()) = %d, want 1", len(unused))
	}
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
