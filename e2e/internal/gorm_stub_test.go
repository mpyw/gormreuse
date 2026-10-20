package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// gormStubOmitted are the methods of the real *gorm.DB that the stub in
// testdata/src/gorm.io/gorm leaves out on purpose, with the reason.
var gormStubOmitted = map[string]string{
	// Promoted from the embedded *gorm.Config. The stub's DB embeds nothing.
	"AfterInitialize": "promoted from *gorm.Config",
	"Apply":           "promoted from *gorm.Config",
	// Dialector and plugin plumbing that application code does not chain.
	"BindVarTo":      "dialector plumbing",
	"DataTypeOf":     "dialector plumbing",
	"DefaultValueOf": "dialector plumbing",
	"Initialize":     "dialector plumbing",
	"Name":           "dialector plumbing",
	"QuoteTo":        "dialector plumbing",
	"Use":            "plugin registration",
	// They do not return *gorm.DB, and no fixture calls them.
	"Explain":        "returns a string",
	"ScanRows":       "returns an error",
	"SetupJoinTable": "returns an error",
	// Chain methods. The analyzer treats a gorm method that returns *gorm.DB
	// and is not Session, WithContext, Debug, Open, Begin or Transaction as a
	// chain method, so these need no fixture of their own.
	"MapColumns": "chain method",
}

// TestGormStubMatchesGorm checks the gorm stub that the analyzer tests load
// against the real gorm of this module. Dependabot bumps the real gorm, not
// the stub, so a method that gorm adds or changes could otherwise be
// misclassified with no failing test (#264). For each method of *gorm.DB it
// compares the name, and the shape of its parameters and results (see
// gormShape): which are a *gorm.DB, and which are callbacks that take or
// return one, since the analyzer finds callback parameters by their declared
// types (#257). A method that the stub omits must be in gormStubOmitted.
// TestGormStubFuncsMatchGorm and TestGormStubSessionMatchesGorm check the
// package-level functions and the Session fields.
func TestGormStubMatchesGorm(t *testing.T) {
	stub := gormStubMethods(t)
	actual := gormRealMethods()

	for _, name := range slices.Sorted(maps.Keys(actual)) {
		stubShape, inStub := stub[name]
		_, omitted := gormStubOmitted[name]
		switch {
		case inStub && omitted:
			t.Errorf("%s is in the stub and in gormStubOmitted; remove it from gormStubOmitted", name)
		case !inStub && !omitted:
			t.Errorf("gorm has (*DB).%s%s, but the stub does not; add it to the stub or to gormStubOmitted", name, actual[name])
		case inStub && stubShape != actual[name]:
			t.Errorf("(*DB).%s is %s in gorm but %s in the stub", name, actual[name], stubShape)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(stub)) {
		if _, ok := actual[name]; !ok {
			t.Errorf("the stub has (*DB).%s, but gorm does not", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(gormStubOmitted)) {
		if _, ok := actual[name]; !ok {
			t.Errorf("gormStubOmitted lists %s, but gorm has no such method; remove it", name)
		}
	}
}

// gormRealFuncs are the package-level functions of the real gorm that the
// stub declares. The analyzer reads gorm.Open as a new root and gorm.Expr as
// a subquery holder, so their shapes must match.
var gormRealFuncs = map[string]any{
	"Expr": gorm.Expr,
	"Open": gorm.Open,
}

// TestGormStubFuncsMatchGorm checks each exported package-level function of
// the stub against the real one of the same name (see gormRealFuncs): the
// shape of its parameters and results.
func TestGormStubFuncsMatchGorm(t *testing.T) {
	stub := gormStubDecls(t, func(fn *ast.FuncDecl) bool { return fn.Recv == nil })
	if len(stub) == 0 {
		t.Fatal("found no package-level function in the stub")
	}
	for _, name := range slices.Sorted(maps.Keys(stub)) {
		real, ok := gormRealFuncs[name]
		if !ok {
			t.Errorf("the stub declares gorm.%s; add the real function to gormRealFuncs", name)
			continue
		}
		if want := gormFuncShape(reflect.TypeOf(real), 0); stub[name] != want {
			t.Errorf("gorm.%s is %s in gorm but %s in the stub", name, want, stub[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(gormRealFuncs)) {
		if _, ok := stub[name]; !ok {
			t.Errorf("gormRealFuncs lists %s, but the stub does not declare it", name)
		}
	}
}

// TestGormStubSessionMatchesGorm checks the fields of the stub's Session
// against the real gorm.Session: each must exist with the same type. Each
// field that the analyzer reads from a Session literal
// (internal/ssa/callsite/session.go) must be in the stub, so the tests cover
// it.
func TestGormStubSessionMatchesGorm(t *testing.T) {
	stub := gormStubStructFields(t, "Session")
	real := reflect.TypeFor[gorm.Session]()
	for _, name := range slices.Sorted(maps.Keys(stub)) {
		f, ok := real.FieldByName(name)
		switch {
		case !ok:
			t.Errorf("the stub's Session has %s, but gorm's does not", name)
		case f.Type.String() != stub[name]:
			t.Errorf("Session.%s is %s in gorm but %s in the stub", name, f.Type, stub[name])
		}
	}
	read := gormSessionFieldsRead(t)
	if len(read) == 0 {
		t.Fatal("found no Session field that the analyzer reads")
	}
	for _, name := range read {
		if _, ok := stub[name]; !ok {
			t.Errorf("the analyzer reads Session.%s, but the stub's Session has no such field", name)
		}
		if _, ok := real.FieldByName(name); !ok {
			t.Errorf("the analyzer reads Session.%s, but gorm's Session has no such field", name)
		}
	}
}

// gormRealMethods returns the shape (see gormShape) of each method of the
// real *gorm.DB, promoted methods included.
func gormRealMethods() map[string]string {
	methods := map[string]string{}
	for m := range reflect.TypeFor[*gorm.DB]().Methods() {
		methods[m.Name] = gormFuncShape(m.Type, 1)
	}
	return methods
}

// gormRealResults returns the result shape of each method of the real
// *gorm.DB, such as "(*DB, other)".
func gormRealResults() map[string]string {
	results := map[string]string{}
	for m := range reflect.TypeFor[*gorm.DB]().Methods() {
		var parts []string
		for out := range m.Type.Outs() {
			parts = append(parts, gormTypeShape(out))
		}
		results[m.Name] = "(" + strings.Join(parts, ", ") + ")"
	}
	return results
}

// gormFuncShape renders the shape of a function type, skipping its first skip
// parameters (the receiver of a method value's type).
func gormFuncShape(fn reflect.Type, skip int) string {
	var params, results []string
	for i := skip; i < fn.NumIn(); i++ {
		in := fn.In(i)
		if fn.IsVariadic() && i == fn.NumIn()-1 {
			params = append(params, "..."+gormTypeShape(in.Elem()))
			continue
		}
		params = append(params, gormTypeShape(in))
	}
	for out := range fn.Outs() {
		results = append(results, gormTypeShape(out))
	}
	return "(" + strings.Join(params, ", ") + ") (" + strings.Join(results, ", ") + ")"
}

// gormTypeShape renders a type as gormShape does.
func gormTypeShape(typ reflect.Type) string {
	switch {
	case typ == reflect.TypeFor[*gorm.DB]():
		return "*DB"
	case typ.Kind() == reflect.Func:
		return "func" + gormFuncShape(typ, 0)
	default:
		return "other"
	}
}

// gormStubMethods returns the shape (see gormShape) of each exported method
// declared on *DB in the stub.
func gormStubMethods(t *testing.T) map[string]string {
	t.Helper()
	return gormStubDecls(t, func(fn *ast.FuncDecl) bool {
		return fn.Recv != nil && gormIsStubDB(fn.Recv.List[0].Type)
	})
}

// gormStubFiles parses the files of the gorm stub.
func gormStubFiles(t *testing.T) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "src", "gorm.io", "gorm", "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("find the gorm stub: %v (found %d files)", err, len(paths))
	}
	var files []*ast.File
	fset := token.NewFileSet()
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse the gorm stub: %v", err)
		}
		files = append(files, f)
	}
	return files
}

// gormStubDecls returns the shape (see gormShape) of each exported function
// of the stub that keep selects.
func gormStubDecls(t *testing.T, keep func(*ast.FuncDecl) bool) map[string]string {
	t.Helper()
	decls := map[string]string{}
	for _, f := range gormStubFiles(t) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.IsExported() && keep(fn) {
				decls[fn.Name.Name] = gormShape(fn.Type)
			}
		}
	}
	return decls
}

// gormStubStructFields returns the type of each field of the stub's struct
// type name, as reflect prints it for the real gorm ("bool",
// "context.Context").
func gormStubStructFields(t *testing.T, name string) map[string]string {
	t.Helper()
	for _, f := range gormStubFiles(t) {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if ts.Name.Name != name || !ok {
					continue
				}
				fields := map[string]string{}
				for _, field := range st.Fields.List {
					typ := types.ExprString(field.Type)
					for _, n := range field.Names {
						fields[n.Name] = typ
					}
				}
				return fields
			}
		}
	}
	t.Fatalf("the gorm stub declares no struct %s", name)
	return nil
}

// gormSessionFieldsRead returns the Session fields that the analyzer reads
// by name: the string keys of fields[...] in internal/ssa/callsite/session.go.
// This module does not import the analyzer, so it reads the source.
func gormSessionFieldsRead(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "ssa", "callsite", "session.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		idx, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		if id, ok := idx.X.(*ast.Ident); !ok || id.Name != "fields" {
			return true
		}
		if lit, ok := idx.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if name, err := strconv.Unquote(lit.Value); err == nil {
				names = append(names, name)
			}
		}
		return true
	})
	slices.Sort(names)
	return slices.Compact(names)
}

// gormShape renders the parameters and results of a function type as
// "(params) (results)". Each is *DB for a *gorm.DB, func(...) (...) for a
// function type, with the same rendering inside, and other for any other
// type. A variadic parameter has a ... prefix. The stub may use another type
// in place of gorm's other types, such as interface{} for *sql.Rows.
func gormShape(fn *ast.FuncType) string {
	var params, results []string
	for _, field := range fn.Params.List {
		for range max(len(field.Names), 1) {
			params = append(params, gormExprShape(field.Type))
		}
	}
	if fn.Results != nil {
		for _, field := range fn.Results.List {
			for range max(len(field.Names), 1) {
				results = append(results, gormExprShape(field.Type))
			}
		}
	}
	return "(" + strings.Join(params, ", ") + ") (" + strings.Join(results, ", ") + ")"
}

// gormExprShape renders a type expression of the stub as gormShape does.
func gormExprShape(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ellipsis:
		return "..." + gormExprShape(e.Elt)
	case *ast.FuncType:
		return "func" + gormShape(e)
	}
	if gormIsStubDB(expr) {
		return "*DB"
	}
	return "other"
}

// gormIsStubDB reports whether expr is *DB in the stub's package.
func gormIsStubDB(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "DB"
}

// gormNonChainBranches are the methods declared on the real *gorm.DB that
// return no *gorm.DB and still branch their receiver, with the reason. Every
// other such method must be in the analyzer's typeutil.receiverUses (#307).
var gormNonChainBranches = map[string]string{
	"Association":    "parses the model into the receiver's Statement and keeps the receiver",
	"AutoMigrate":    "runs Migrator",
	"Connection":     "runs its callback on the receiver itself when it is clone==0",
	"Migrator":       "runs pending scopes in place and keeps a copy that shares the Statement (TestStateMethods_MigratorSharesStatement)",
	"Row":            "runs a query",
	"Rows":           "runs a query",
	"ScanRows":       "sets Statement.Dest and scans into it",
	"SetupJoinTable": "parses models into the receiver's Statement",
	"Use":            "hands the receiver to a plugin",
}

// TestGormReceiverUsesMatchGorm checks the analyzer's table of gorm methods
// that do not branch their receiver (typeutil.receiverUses) against the real
// *gorm.DB (#307). Each method declared on *gorm.DB that returns no *gorm.DB
// must be in that table or in gormNonChainBranches, and every name in either
// must be a method declared on *gorm.DB (or gorm.Open). Methods promoted from
// the embedded *gorm.Config are calls on the Config, not uses of the *gorm.DB,
// so they are in neither.
func TestGormReceiverUsesMatchGorm(t *testing.T) {
	uses := gormReceiverUses(t)
	results := gormRealResults()
	declared := map[string]bool{}
	for m := range reflect.TypeFor[*gorm.DB]().Methods() {
		fn := runtime.FuncForPC(m.Func.Pointer())
		if file, _ := fn.FileLine(fn.Entry()); file == "<autogenerated>" {
			continue // promoted from *gorm.Config
		}
		declared[m.Name] = true
		if strings.Contains(results[m.Name], "*DB") {
			continue // a chain method, or Session and the like
		}
		inUses, branches := uses[m.Name], gormNonChainBranches[m.Name] != ""
		switch {
		case inUses && branches:
			t.Errorf("(*DB).%s is in receiverUses and in gormNonChainBranches; keep one", m.Name)
		case !inUses && !branches:
			t.Errorf("(*DB).%s returns no *gorm.DB; add it to typeutil.receiverUses if it does not branch its receiver, or to gormNonChainBranches with the reason", m.Name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(uses)) {
		if name != "Open" && !declared[name] {
			t.Errorf("typeutil.receiverUses lists %s, but *gorm.DB declares no such method", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(gormNonChainBranches)) {
		if !declared[name] {
			t.Errorf("gormNonChainBranches lists %s, but *gorm.DB declares no such method", name)
		}
	}
}

// gormReceiverUses returns the keys of typeutil.receiverUses, read from the
// analyzer's source: this module does not import the analyzer.
func gormReceiverUses(t *testing.T) map[string]bool {
	t.Helper()
	return gormStringKeys(t, filepath.Join("..", "..", "internal", "typeutil", "gorm.go"), "receiverUses")
}

// gormStringKeys returns the string keys of the map literal that the
// package-level variable name holds in the analyzer's file path.
func gormStringKeys(t *testing.T, path, name string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	keys := map[string]bool{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Names) != 1 || vs.Names[0].Name != name || len(vs.Values) != 1 {
				continue
			}
			lit, _ := vs.Values[0].(*ast.CompositeLit)
			if lit == nil {
				t.Fatalf("%s in %s is not a map literal", name, path)
			}
			for _, elt := range lit.Elts {
				key := elt.(*ast.KeyValueExpr).Key.(*ast.BasicLit)
				k, err := strconv.Unquote(key.Value)
				if err != nil {
					t.Fatalf("%s key %s: %v", name, key.Value, err)
				}
				keys[k] = true
			}
		}
	}
	if len(keys) == 0 {
		t.Fatalf("find %s in %s", name, path)
	}
	return keys
}

// statementUncloned are the fields of gorm.Statement that Statement.clone()
// does not copy, with what sets them. The analyzer acts on the ones that a
// chain method sets: attrs (Attrs) and assigns (Assign).
var statementUncloned = map[string]string{
	"DB":           "the clone points at its new *gorm.DB",
	"BuildClauses": "set by a finisher's callbacks while they build the SQL",
	"CurDestIndex": "set while rows are scanned",
	"attrs":        "set by Attrs, read by FirstOrInit and FirstOrCreate",
	"assigns":      "set by Assign, read by FirstOrInit and FirstOrCreate",
}

// statementCloned are the fields that Statement.clone() copies, with a note
// where the copy is partial.
var statementCloned = map[string]string{
	"TableExpr":            "",
	"Table":                "",
	"Model":                "",
	"Unscoped":             "",
	"Dest":                 "",
	"ReflectValue":         "",
	"Clauses":              "",
	"Distinct":             "",
	"Selects":              "",
	"Omits":                "",
	"ColumnMapping":        "",
	"Joins":                "",
	"Preloads":             "",
	"Settings":             "each entry, but InstanceSet's keys hold the old Statement's address (TestUnclonedState_InstanceSetLost)",
	"ConnPool":             "",
	"Schema":               "",
	"Context":              "",
	"RaiseErrorOnNotFound": "",
	"SkipHooks":            "",
	"SQL":                  "",
	"Vars":                 "only when SQL is not empty",
	"scopes":               "",
	"Result":               "",
}

// TestGormStatementFieldsClassified checks statementCloned and
// statementUncloned against the real gorm.Statement: every field is in
// exactly one (#344). A field that gorm adds fails the test until it is
// classified, so a new field that a Session drops is noticed when Dependabot
// bumps gorm. TestUnclonedState_StatementFields checks the lists at run time.
// Each method of the analyzer's unclonedStateMethods must be a method of
// *gorm.DB and of the stub.
func TestGormStatementFieldsClassified(t *testing.T) {
	fields := map[string]bool{}
	for f := range reflect.TypeFor[gorm.Statement]().Fields() {
		fields[f.Name] = true
		_, cloned := statementCloned[f.Name]
		_, uncloned := statementUncloned[f.Name]
		switch {
		case cloned && uncloned:
			t.Errorf("Statement.%s is in statementCloned and statementUncloned; keep one", f.Name)
		case !cloned && !uncloned:
			t.Errorf("gorm.Statement has %s, which neither list classifies; check whether Statement.clone() copies it, and whether a chain method sets it (#344)", f.Name)
		}
	}
	for _, list := range []map[string]string{statementCloned, statementUncloned} {
		for _, name := range slices.Sorted(maps.Keys(list)) {
			if !fields[name] {
				t.Errorf("a list of Statement fields has %s, but gorm.Statement has no such field", name)
			}
		}
	}

	methods := gormStringKeys(t, filepath.Join("..", "..", "internal", "ssa", "tracer", "state.go"), "unclonedStateMethods")
	stub, real := gormStubMethods(t), gormRealMethods()
	for _, name := range slices.Sorted(maps.Keys(methods)) {
		if _, ok := real[name]; !ok {
			t.Errorf("unclonedStateMethods lists %s, but *gorm.DB has no such method", name)
		}
		if _, ok := stub[name]; !ok {
			t.Errorf("unclonedStateMethods lists %s, but the stub does not declare it", name)
		}
	}
}
