// Command gormreuse is a static analysis tool for detecting unsafe
// *gorm.DB instance reuse in Go code.
//
// Usage:
//
//	gormreuse ./...
//
// Or as a vet tool:
//
//	go vet -vettool=$(which gormreuse) ./...
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/mpyw/gormreuse"
)

func main() {
	singlechecker.Main(gormreuse.Analyzer)
}
