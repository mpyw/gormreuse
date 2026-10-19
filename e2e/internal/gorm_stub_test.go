package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
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
	"Callback":       "callback registration",
	// They do not return *gorm.DB, and no fixture calls them.
	"Explain":        "returns a string",
	"ToSQL":          "returns a string",
	"Get":            "returns a value and a bool",
	"InstanceGet":    "returns a value and a bool",
	"ScanRows":       "returns an error",
	"SetupJoinTable": "returns an error",
	// Chain methods. The analyzer treats a gorm method that returns *gorm.DB
	// and is not Session, WithContext, Debug, Open, Begin or Transaction as a
	// chain method, so these need no fixture of their own.
	"InstanceSet": "chain method",
	"MapColumns":  "chain method",
	"Set":         "chain method",
}

// TestGormStubMatchesGorm checks the gorm stub that the analyzer tests load
// against the real gorm of this module. Dependabot bumps the real gorm, not
// the stub, so a method that gorm adds or changes could otherwise be
// misclassified with no failing test (#264). For each method of *gorm.DB it
// compares the name and, for each result, whether it is a *gorm.DB. A method
// that the stub omits must be in gormStubOmitted.
func TestGormStubMatchesGorm(t *testing.T) {
	stub := gormStubMethods(t)
	actual := gormRealMethods()

	for _, name := range slices.Sorted(maps.Keys(actual)) {
		stubResults, inStub := stub[name]
		_, omitted := gormStubOmitted[name]
		switch {
		case inStub && omitted:
			t.Errorf("%s is in the stub and in gormStubOmitted; remove it from gormStubOmitted", name)
		case !inStub && !omitted:
			t.Errorf("gorm has (*DB).%s returning %s, but the stub does not; add it to the stub or to gormStubOmitted", name, actual[name])
		case inStub && stubResults != actual[name]:
			t.Errorf("(*DB).%s returns %s in gorm but %s in the stub", name, actual[name], stubResults)
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

// gormRealMethods returns the result shape (see gormResultShape) of each
// method of the real *gorm.DB, promoted methods included.
func gormRealMethods() map[string]string {
	dbType := reflect.TypeFor[*gorm.DB]()
	methods := map[string]string{}
	for m := range dbType.Methods() {
		var isDB []bool
		for out := range m.Type.Outs() {
			isDB = append(isDB, out == dbType)
		}
		methods[m.Name] = gormResultShape(isDB)
	}
	return methods
}

// gormStubMethods returns the result shape (see gormResultShape) of each
// exported method declared on *DB in the stub.
func gormStubMethods(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "src", "gorm.io", "gorm", "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("find the gorm stub: %v (found %d files)", err, len(paths))
	}
	methods := map[string]string{}
	fset := token.NewFileSet()
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse the gorm stub: %v", err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() || !gormIsStubDB(fn.Recv.List[0].Type) {
				continue
			}
			var isDB []bool
			if fn.Type.Results != nil {
				for _, field := range fn.Type.Results.List {
					for range max(len(field.Names), 1) {
						isDB = append(isDB, gormIsStubDB(field.Type))
					}
				}
			}
			methods[fn.Name.Name] = gormResultShape(isDB)
		}
	}
	return methods
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

// gormResultShape renders a result list as "(*DB, other)": *DB for a
// *gorm.DB result and other for any other type. The stub may use another type
// in place of gorm's other results, such as interface{} for *sql.Rows.
func gormResultShape(isDB []bool) string {
	parts := make([]string, len(isDB))
	for i, db := range isDB {
		parts[i] = "other"
		if db {
			parts[i] = "*DB"
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
