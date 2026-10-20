package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// factsLib declares directives for factsUse, which is in another package.
const factsLib = `package lib

import "gorm.io/gorm"

//gormreuse:immutable-input(cb)
func WithFresh(db *gorm.DB, cb func(*gorm.DB)) { cb(db.Session(&gorm.Session{})) }

func SameLine(db *gorm.DB) { //gormreuse:pure
	_ = db
}

//gormreuse:pure
func Generic[T any](db *gorm.DB, v T) { _, _ = db, v }

//gormreuse:pure
func BadPure(db *gorm.DB) {
	db.Where("x")
}

//gormreuse:immutable-param
func ApplyFilters(tx *gorm.DB) {
	tx.Where("a").Find(nil)
	tx.Where("b").Find(nil)
}
`

// factsUse calls the functions of factsLib.
const factsUse = `package use

import (
	"gorm.io/gorm"

	"example.com/repro/lib"
)

func callback(db *gorm.DB) {
	lib.WithFresh(db, func(tx *gorm.DB) {
		tx.Find(nil)
		tx.Count(nil)
	})
}

func sameLineAndGeneric(db *gorm.DB) {
	q := db.Where("x")
	lib.SameLine(q)
	lib.Generic[int](q, 1)
	q.Find(nil)
}

func badPure(db *gorm.DB) {
	q := db.Where("x")
	lib.BadPure(q)
	q.Find(nil)
}

func immutableParam(db *gorm.DB) {
	lib.ApplyFilters(db.Where("x"))
}
`

// TestCrossPackageFactsE2E builds the command and checks that directives
// declared in one package apply in another, through analysis facts (#195),
// both when the command runs as a singlechecker and as a go vet tool. The
// module replaces gorm.io/gorm with the testdata stub, so no network is
// needed.
func TestCrossPackageFactsE2E(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	bin := filepath.Join(t.TempDir(), "gormreuse")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	stub, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "testdata", "src", "gorm.io", "gorm", "gorm.go"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"go.mod":           "module example.com/repro\n\ngo 1.27.0\n\nrequire gorm.io/gorm v0.0.0\n\nreplace gorm.io/gorm => ./gormstub\n",
		"gormstub/go.mod":  "module gorm.io/gorm\n\ngo 1.27.0\n",
		"gormstub/gorm.go": string(stub),
		"lib/lib.go":       factsLib,
		"use/use.go":       factsUse,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The use package's diagnostics only: BadPure is not trusted, and
	// ApplyFilters needs an isolated value. tx in the WithFresh callback, and
	// q after the same-line and generic pure calls, are not reported.
	want := []string{
		"use.go:26:8: *gorm.DB reused: second branch from mutable root",
		"use.go:30:18: mutable *gorm.DB passed to ApplyFilters, which has a gormreuse:immutable-param directive",
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"singlechecker", []string{bin, "./use"}},
		{"go vet", []string{"go", "vet", "-vettool=" + bin, "./use"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(tc.args[0], tc.args[1:]...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off")
			out, _ := cmd.CombinedOutput()

			// Each diagnostic line starts with the file's path, which ends in
			// use.go. The message names use.go again, so cut at the first.
			var got []string
			for line := range strings.SplitSeq(string(out), "\n") {
				if _, diag, found := strings.Cut(line, "use.go:"); found {
					got = append(got, "use.go:"+diag)
				}
			}
			ok := len(got) == len(want)
			for i := 0; ok && i < len(want); i++ {
				ok = strings.HasPrefix(got[i], want[i])
			}
			if !ok {
				t.Errorf("output:\n%s\nwant exactly the diagnostics starting with:\n%s", out, strings.Join(want, "\n"))
			}
		})
	}
}
