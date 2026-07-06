package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSmoke builds the vettool and runs it against the known-bad gormreuse
// fixture package, asserting it exits non-zero and prints the expected
// diagnostic (issue #77 item 4). This is the only coverage of main.go's wiring
// of singlechecker; the analysis itself is covered by the analysistest suite.
func TestSmoke(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	bin := filepath.Join(t.TempDir(), "gormreuse")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// The fixtures live under the module's testdata GOPATH root.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	testdata := filepath.Join(filepath.Dir(file), "..", "..", "testdata")

	cmd := exec.Command(bin, "gormreuse")
	cmd.Dir = testdata
	cmd.Env = append(os.Environ(), "GOPATH="+testdata, "GO111MODULE=off")
	out, err := cmd.CombinedOutput()

	// The vettool exits non-zero when it reports diagnostics.
	if err == nil {
		t.Errorf("expected non-zero exit (diagnostics reported), got success\n%s", out)
	}
	if !strings.Contains(string(out), "reused after chain method") {
		t.Errorf("expected 'reused after chain method' diagnostic, got:\n%s", out)
	}
}
