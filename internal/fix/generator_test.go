package fix

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestIsFinisher(t *testing.T) {
	tests := []struct {
		name       string
		methodName string
		want       bool
	}{
		// Query execution finishers
		{"Find is finisher", "Find", true},
		{"First is finisher", "First", true},
		{"Last is finisher", "Last", true},
		{"Take is finisher", "Take", true},
		{"Count is finisher", "Count", true},
		{"Pluck is finisher", "Pluck", true},
		{"Scan is finisher", "Scan", true},
		{"Row is finisher", "Row", true},
		{"Rows is finisher", "Rows", true},
		{"ScanRows is finisher", "ScanRows", true},

		// Data manipulation finishers (semantic)
		{"Create is finisher", "Create", true},
		{"Save is finisher", "Save", true},
		{"Update is finisher", "Update", true},
		{"Updates is finisher", "Updates", true},
		{"Delete is finisher", "Delete", true},

		// Other finishers
		{"Exec is finisher", "Exec", true},
		{"Transaction is finisher", "Transaction", true},

		// Chain methods (NOT finishers)
		{"Where is NOT finisher", "Where", false},
		{"Or is NOT finisher", "Or", false},
		{"Not is NOT finisher", "Not", false},
		{"Select is NOT finisher", "Select", false},
		{"Omit is NOT finisher", "Omit", false},
		{"Joins is NOT finisher", "Joins", false},
		{"Group is NOT finisher", "Group", false},
		{"Having is NOT finisher", "Having", false},
		{"Order is NOT finisher", "Order", false},
		{"Limit is NOT finisher", "Limit", false},
		{"Offset is NOT finisher", "Offset", false},
		{"Scopes is NOT finisher", "Scopes", false},
		{"Preload is NOT finisher", "Preload", false},
		{"Distinct is NOT finisher", "Distinct", false},
		{"Unscoped is NOT finisher", "Unscoped", false},
		{"Table is NOT finisher", "Table", false},
		{"Model is NOT finisher", "Model", false},
		{"Clauses is NOT finisher", "Clauses", false},
		{"Assign is NOT finisher", "Assign", false},
		{"Attrs is NOT finisher", "Attrs", false},
		{"InnerJoins is NOT finisher", "InnerJoins", false},

		// Random non-GORM methods
		{"Random method is NOT finisher", "DoSomething", false},
		{"Empty string is NOT finisher", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFinisher(tt.methodName); got != tt.want {
				t.Errorf("isFinisher(%q) = %v, want %v", tt.methodName, got, tt.want)
			}
		})
	}
}

func TestIsDirectOrSelectorOf(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want bool
	}{
		{
			name: "Direct match - expr is target",
			expr: "q.Where(\"x\")",
			want: true,
		},
		{
			name: "Selector of target - .Error",
			expr: "q.Where(\"x\").Error",
			want: true,
		},
		{
			name: "Selector chain of target",
			expr: "q.Where(\"x\").Find(nil).Error",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse the expression
			fset := token.NewFileSet()
			exprNode, err := parser.ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("Failed to parse expr %q: %v", tt.expr, err)
			}

			// Find the first CallExpr in the parsed expression
			var targetCall *ast.CallExpr
			ast.Inspect(exprNode, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && targetCall == nil {
					targetCall = call
					return false
				}
				return true
			})

			if targetCall == nil {
				t.Fatalf("No CallExpr found in %q", tt.expr)
			}

			g := &Generator{fset: fset}
			if got := g.isDirectOrSelectorOf(exprNode, targetCall); got != tt.want {
				t.Errorf("isDirectOrSelectorOf(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestFindInnermostCallExpr(t *testing.T) {
	tests := []struct {
		name       string
		code       string
		searchCall string // Method name to search for (e.g., "Where", "Save")
		want       string // Expected innermost call at that position
	}{
		{
			name: "Simple call",
			code: `package test
func f() {
	q.Where("x")
}`,
			searchCall: "Where",
			want:       "Where",
		},
		{
			name: "Nested in function call",
			code: `package test
func f() {
	require.NoError(t, db.Save(&x).Error)
}`,
			searchCall: "Save",
			want:       "Save",
		},
		{
			name: "Nested in method call",
			code: `package test
func f() {
	helper.TruncateTables(db.Where("x"))
}`,
			searchCall: "Where",
			want:       "Where",
		},
		{
			name: "Doubly nested",
			code: `package test
func f() {
	outerFunc(require.NoError(t, db.Save(&x).Error))
}`,
			searchCall: "Save",
			want:       "Save",
		},
		{
			name: "Chained call",
			code: `package test
func f() {
	q.Where("x").Order("y")
}`,
			searchCall: "Where",
			want:       "Where",
		},
		{
			name: "Chained with selector",
			code: `package test
func f() {
	_ = q.Where("x").Find(nil).Error
}`,
			searchCall: "Where",
			want:       "Where",
		},
		{
			name: "Method chain in function argument",
			code: `package test
func f() {
	helper(q.Where("x").Order("y").Find(nil))
}`,
			searchCall: "Where",
			want:       "Where",
		},
		{
			name: "Multiple gorm calls - first",
			code: `package test
func f() {
	q.Where("x")
	q.Order("y")
}`,
			searchCall: "Where",
			want:       "Where",
		},
		{
			name: "Multiple gorm calls - second",
			code: `package test
func f() {
	q.Where("x")
	q.Order("y")
}`,
			searchCall: "Order",
			want:       "Order",
		},
		{
			name: "Argument contains function call",
			code: `package test
func f() {
	q.Where(foo.Bar())
}`,
			searchCall: "Where",
			want:       "Where",
		},
		{
			name: "Multiple nested calls in arguments",
			code: `package test
func f() {
	q.Where(helper1(helper2(x)))
}`,
			searchCall: "Where",
			want:       "Where",
		},
		{
			name: "Method chain with function call arguments",
			code: `package test
func f() {
	q.Where(foo.Bar()).Order(baz.Qux())
}`,
			searchCall: "Where",
			want:       "Where",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "test.go", tt.code, 0)
			if err != nil {
				t.Fatalf("Failed to parse code: %v", err)
			}

			// Find the CallExpr for the search method
			var targetPos token.Pos
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if sel.Sel.Name == tt.searchCall {
							targetPos = call.Pos()
							return false
						}
					}
				}
				return true
			})

			if targetPos == token.NoPos {
				t.Fatalf("Could not find call to %q in code", tt.searchCall)
			}

			// Build token.File -> ast.File mapping
			files := make(map[*token.File]*ast.File)
			files[fset.File(file.Pos())] = file

			g := &Generator{
				fset:  fset,
				files: files,
			}

			call := g.findInnermostCallExpr(file, targetPos)
			if call == nil {
				t.Fatalf("findInnermostCallExpr() = nil, want call")
			}

			// Check if the call matches expected name
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				got := sel.Sel.Name
				if got != tt.want {
					t.Errorf("findInnermostCallExpr() = %q, want %q", got, tt.want)
				}
			} else {
				t.Errorf("findInnermostCallExpr() returned non-selector call")
			}
		})
	}
}

func TestExtractAssignableLHS(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want string
	}{
		{"Simple identifier", "q", "q"},
		{"Struct field", "c.DB", "c.DB"},
		{"Nested struct field", "n.Container.DB", "n.Container.DB"},
		{"Array index", "arr[0]", ""},       // Not supported
		{"Map index", "m[\"key\"]", ""},     // Not supported
		{"Pointer deref", "(*ptr)", "*ptr"}, // Star expr supported
		{"Complex expr", "f().Field", ""},   // Not supported
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			exprNode, err := parser.ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("Failed to parse expr %q: %v", tt.expr, err)
			}

			g := &Generator{fset: fset}
			if got := g.extractAssignableLHS(exprNode); got != tt.want {
				t.Errorf("extractAssignableLHS(%q) = %q, want %q", tt.expr, got, tt.want)
			}
		})
	}
}

func TestVirtualRootKey(t *testing.T) {
	// Test that virtualRootKey can be used as map key
	m := make(map[virtualRootKey]int)

	k1 := virtualRootKey{isOriginal: true, pos: token.Pos(100)}
	k2 := virtualRootKey{isOriginal: false, pos: token.Pos(200)}
	k3 := virtualRootKey{isOriginal: true, pos: token.Pos(100)} // Same as k1

	m[k1] = 1
	m[k2] = 2
	m[k3] = 3 // Should overwrite k1

	if len(m) != 2 {
		t.Errorf("Expected map length 2, got %d", len(m))
	}

	if m[k1] != 3 {
		t.Errorf("Expected m[k1] = 3, got %d", m[k1])
	}

	if m[k2] != 2 {
		t.Errorf("Expected m[k2] = 2, got %d", m[k2])
	}
}
