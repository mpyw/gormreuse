// Package clause is a stub for testing purposes.
package clause

// Expr is a raw SQL expression with its variables.
type Expr struct {
	SQL                string
	Vars               []interface{}
	WithoutParentheses bool
}
