//declscope:namespace main

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// TestBuildVersion checks where -V=full takes the version from (#351).
func TestBuildVersion(t *testing.T) {
	t.Parallel()
	module := &debug.BuildInfo{Main: debug.Module{Version: "v0.23.0"}}
	tests := []struct {
		name, linked string
		info         *debug.BuildInfo
		want         string
	}{
		{"linked", "v0.24.0", module, "v0.24.0"},
		{"module", "", module, "v0.23.0"},
		{"local build", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "(devel)"},
		{"no version", "", &debug.BuildInfo{}, "(devel)"},
		{"no build info", "", nil, "(devel)"},
	}
	for _, tt := range tests {
		if got := buildVersion(tt.linked, tt.info); got != tt.want {
			t.Errorf("%s: buildVersion = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// versionFull matches a -V=full line that "go vet" accepts: "devel" and a
// buildID, so that vet keys its cache on the content of the binary.
var versionFull = regexp.MustCompile(`^(\S+) version devel (\S+) buildID=[0-9a-f]{64}\n$`)

// TestPrintVersion checks the -V=full line of the test binary, and the error
// for a binary that cannot be read.
func TestPrintVersion(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if err := printVersion(&b, os.Args[0]); err != nil {
		t.Fatal(err)
	}
	if m := versionFull.FindStringSubmatch(b.String()); m == nil || m[1] != os.Args[0] {
		t.Errorf("printVersion = %q, want %s version devel <version> buildID=<sha256>", b.String(), os.Args[0])
	}
	if err := printVersion(&b, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("printVersion of a missing binary: no error")
	}
}

// TestVersionFlagValue checks the flag.Value methods of versionFlag, with
// the output and the exit of -V=full replaced. It does not run in parallel,
// since it replaces them for the whole package.
func TestVersionFlagValue(t *testing.T) {
	var v versionFlag
	if !v.IsBoolFlag() || v.Get() != nil || v.String() != "" {
		t.Errorf("IsBoolFlag, Get, String = %v, %v, %q; want true, nil, \"\"", v.IsBoolFlag(), v.Get(), v.String())
	}
	if err := v.Set("short"); err == nil {
		t.Error("Set(\"short\"): no error")
	}

	var b strings.Builder
	exit := -1
	versionOutput, versionExit = &b, func(code int) { exit = code }
	defer func() { versionOutput, versionExit = os.Stdout, os.Exit }()
	if err := v.Set("full"); err != nil {
		t.Fatal(err)
	}
	if exit != 0 || !versionFull.MatchString(b.String()) {
		t.Errorf("Set(\"full\") printed %q and exited %d, want a -V=full line and 0", b.String(), exit)
	}
}

// TestVersionFlag builds the command as the release build does, with the
// version linked in, and checks that -V=full prints it (#351).
func TestVersionFlag(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	bin := filepath.Join(t.TempDir(), "gormreuse")
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "-V=full").Output()
	if err != nil {
		t.Fatalf("-V=full: %v", err)
	}
	if m := versionFull.FindStringSubmatch(string(out)); m == nil || m[2] != "v9.9.9" {
		t.Errorf("-V=full = %q, want the version v9.9.9", out)
	}
	if err := exec.Command(bin, "-V=short").Run(); err == nil {
		t.Error("-V=short: exit 0, want an error")
	}
}
