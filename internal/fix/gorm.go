package fix

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/typeutil"
)

// gormNameAt returns the selector prefix that refers to gorm.io/gorm at every
// one of positions in file: "gorm." for a plain import, "<alias>." for an
// aliased one, or "" for a dot import. Each name is resolved in the scope at
// the position, so a parameter or local variable named gorm that shadows the
// import there is seen (#200).
//
// When no import of the file can be used there, importName is the name of an
// import to add: "gorm", or "gormio", "gormio2" and so on when gorm is taken,
// such as by another package imported as gorm. A blank import has no usable
// name.
//
//declscope:shared // session.go's sessionEdits qualifies each Session with it
func (g *Generator) gormNameAt(file *ast.File, positions []token.Pos) (qualifier, importName string) {
	resolvesAt := func(name string, want func(types.Object) bool) bool {
		for _, pos := range positions {
			_, obj := g.pass.Pkg.Scope().Innermost(pos).LookupParent(name, pos)
			if !want(obj) {
				return false
			}
		}
		return true
	}
	isGorm := func(pkg *types.Package) bool { return pkg != nil && pkg.Path() == gormImportPath }
	for _, imp := range file.Imports {
		pkgName := g.pass.TypesInfo.PkgNameOf(imp)
		if pkgName == nil || !isGorm(pkgName.Imported()) {
			continue
		}
		switch pkgName.Name() {
		case "_":
		case ".":
			if resolvesAt("Session", func(obj types.Object) bool { return obj != nil && isGorm(obj.Pkg()) }) {
				return "", ""
			}
		default:
			if resolvesAt(pkgName.Name(), func(obj types.Object) bool { return obj == pkgName }) {
				return pkgName.Name() + ".", ""
			}
		}
	}
	isFree := func(obj types.Object) bool { return obj == nil }
	name := "gorm"
	for i := 1; !resolvesAt(name, isFree); i++ {
		name = "gormio"
		if i > 1 {
			name += strconv.Itoa(i)
		}
	}
	return name + ".", name
}

const gormImportPath = "gorm.io/gorm"

// gormImportEdit adds an import of gorm.io/gorm named name to file. A name
// other than gorm is written as an alias.
//
//declscope:shared // session.go's sessionEdits adds the import with it
func gormImportEdit(file *ast.File, name string) analysis.TextEdit {
	spec := strconv.Quote(gormImportPath)
	if name != "gorm" {
		spec = name + " " + spec
	}

	// Find existing import declaration
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.IMPORT || len(genDecl.Specs) == 0 {
			continue
		}

		// Check if it's a grouped import: import ( ... )
		if genDecl.Lparen.IsValid() {
			// Grouped import - append after last spec
			lastSpec := genDecl.Specs[len(genDecl.Specs)-1]
			return analysis.TextEdit{
				Pos:     lastSpec.End(),
				End:     lastSpec.End(),
				NewText: []byte("\n\t" + spec),
			}
		}

		// Single import - add new import line after this declaration
		return analysis.TextEdit{
			Pos:     genDecl.End(),
			End:     genDecl.End(),
			NewText: []byte("\nimport " + spec),
		}
	}

	// No import declaration found - insert after package clause
	return analysis.TextEdit{
		Pos:     file.Name.End(),
		End:     file.Name.End(),
		NewText: []byte("\n\nimport " + spec),
	}
}

// isGormFinisher checks if a method name is a GORM finisher method.
//
//declscope:shared // reassign.go's discardedChainToReassign and change.go's carriesRootChanges stop at a finisher
func isGormFinisher(methodName string) bool {
	finishers := map[string]bool{
		"Find":          true,
		"First":         true,
		"Last":          true,
		"Take":          true,
		"Count":         true,
		"Pluck":         true,
		"Scan":          true,
		"Row":           true,
		"Rows":          true,
		"ScanRows":      true,
		"Create":        true,
		"Save":          true,
		"Update":        true,
		"Updates":       true,
		"Delete":        true,
		"Exec":          true,
		"Transaction":   true,
		"FirstOrCreate": true, // terminal (executes); #71 secondary
		"FirstOrInit":   true, // terminal (executes); #71 secondary
		// These return *gorm.DB but run SQL or end a transaction, so a
		// discarded call is not a chain to reassign (#200).
		"CreateInBatches": true,
		"FindInBatches":   true,
		"UpdateColumn":    true,
		"UpdateColumns":   true,
		"Commit":          true,
		"Rollback":        true,
		"SavePoint":       true,
		"RollbackTo":      true,
	}
	return finishers[methodName]
}

// isGormStateField reports whether fa takes the Error, RowsAffected or
// Statement field of a *gorm.DB.
//
//declscope:shared // change.go's changedElsewhere finds the reads of the root's state with it
func isGormStateField(fa *ssa.FieldAddr) bool {
	if !typeutil.IsGormDB(fa.X.Type()) {
		return false
	}
	name := fa.X.Type().Underlying().(*types.Pointer).Elem().Underlying().(*types.Struct).Field(fa.Field).Name()
	return name == "Error" || name == "RowsAffected" || name == "Statement"
}
