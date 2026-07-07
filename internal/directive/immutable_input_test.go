package directive

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// parseFileWithTypes parses src as a single Go file in package "demo" and runs
// go/types over it so the resulting *ast.File / *types.Info combo can drive
// ImmutableInputSet.AddFile in tests.
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
	// Every accessor must tolerate a nil receiver — the analyzer passes a nil set
	// when directive bookkeeping isn't applicable.
	nilSet.AddFile(nil, "demo")
	if got := nilSet.Callbacks(nil); got != nil {
		t.Errorf("Callbacks(nil) = %v, want nil", got)
	}
	if got := nilSet.GetUnused(); got != nil {
		t.Errorf("GetUnused() = %v, want nil", got)
	}
}

func TestImmutableInputSet_AddFile_NoDirectives(t *testing.T) {
	t.Parallel()

	const src = "package demo\n\nfunc plain(cb func()) {}\n"
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	if got := len(s.GetUnused()); got != 0 {
		t.Errorf("GetUnused() with no directives = %d, want 0", got)
	}
}

func TestImmutableInputSet_AddFile_ParamNotFound(t *testing.T) {
	t.Parallel()

	const src = "package demo\n\n//gormreuse:immutable-input(missing)\nfunc noSuchParam(cb func()) {}\n"
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	assertOneUnused(t, s, `parameter "missing" not found`)
}

func TestImmutableInputSet_AddFile_ParamNotFunctionType(t *testing.T) {
	t.Parallel()

	const src = "package demo\n\n//gormreuse:immutable-input(x)\nfunc notFn(x int) {}\n"
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	assertOneUnused(t, s, `parameter "x" is not a function type`)
}

// U3: the named parameter is a function type, but its signature has no *gorm.DB
// argument, so the directive can never apply.
func TestImmutableInputSet_AddFile_CallbackHasNoGormDB(t *testing.T) {
	t.Parallel()

	const src = "package demo\n\n//gormreuse:immutable-input(cb)\nfunc cbNoGormDB(cb func(int) int) {}\n"
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	assertOneUnused(t, s, "no *gorm.DB parameter")
}

// Covers the lookupParam branch where a field has no Names slice — it still
// consumes a position but cannot match any name — and confirms the index is
// advanced so a later parameter still resolves.
func TestImmutableInputSet_AddFile_AnonymousParam(t *testing.T) {
	t.Parallel()

	const src = "package demo\n\n//gormreuse:immutable-input(missing)\nfunc anon(int, cb func()) {}\n"
	fset, file, info := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, info)
	s.AddFile(file, "demo")
	assertOneUnused(t, s, `parameter "missing" not found`)
}

// With no type info, lookupParam returns a nil type, so asFunctionSignature(nil)
// takes its nil branch and the directive is honestly reported as U2 rather than
// synthesising a misleading U3.
func TestImmutableInputSet_AddFile_NilTypesInfo(t *testing.T) {
	t.Parallel()

	const src = "package demo\n\n//gormreuse:immutable-input(cb)\nfunc noTypeInfo(cb func()) {}\n"
	fset, file, _ := parseFileWithTypes(t, src)
	s := NewImmutableInputSet(fset, nil) // no TypesInfo
	s.AddFile(file, "demo")
	assertOneUnused(t, s, `parameter "cb" is not a function type`)
}

func assertOneUnused(t *testing.T, s *ImmutableInputSet, want string) {
	t.Helper()
	unused := s.GetUnused()
	if len(unused) != 1 {
		t.Fatalf("len(GetUnused()) = %d, want 1; entries=%+v", len(unused), unused)
	}
	if got := unused[0].Reason; !strings.Contains(got, want) {
		t.Errorf("Reason = %q, want substring %q", got, want)
	}
}
