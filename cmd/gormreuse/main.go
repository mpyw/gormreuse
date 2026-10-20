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
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/mpyw/gormreuse"
)

// version is the release version. The release build sets it with
// -ldflags "-X main.version=vX.Y.Z" (see .goreleaser.yaml).
var version string

func main() {
	// singlechecker adds its own -V only when the flag is not defined yet.
	flag.Var(versionFlag{}, "V", "print version and exit")
	singlechecker.Main(gormreuse.Analyzer)
}

// versionFlag is the -V=full flag of the "go vet" protocol, as singlechecker
// writes it, with the version of gormreuse in the line (#351). singlechecker
// prints "version devel comments-go-here" for every build.
type versionFlag struct{}

func (versionFlag) IsBoolFlag() bool { return true }
func (versionFlag) Get() any         { return nil }
func (versionFlag) String() string   { return "" }

func (versionFlag) Set(s string) error {
	if s != "full" {
		return fmt.Errorf("unsupported flag value: -V=%s (use -V=full)", s)
	}
	progname, err := os.Executable()
	if err != nil {
		return err
	}
	if err := printVersion(os.Stdout, progname); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}

// printVersion writes the -V=full line of the binary at progname to w.
func printVersion(w io.Writer, progname string) error {
	bin, err := os.ReadFile(progname)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	info, _ := debug.ReadBuildInfo()
	_, err = fmt.Fprintln(w, versionLine(progname, buildVersion(version, info), sum[:]))
	return err
}

// versionLine renders the -V=full output. It keeps "devel" and the buildID
// that "go vet" reads, so that vet keys its cache on the content of the
// binary, and puts the version where singlechecker writes
// "comments-go-here".
func versionLine(progname, version string, sum []byte) string {
	return fmt.Sprintf("%s version devel %s buildID=%02x", progname, version, sum)
}

// buildVersion returns the version that the build gives: the one the linker
// set, else the module version that go install records, else the VCS
// pseudo-version that go build records, else "(devel)".
func buildVersion(linked string, info *debug.BuildInfo) string {
	if linked != "" {
		return linked
	}
	if info != nil && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
