package internal_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
	"github.com/mpyw/gormreuse/internal"
	"github.com/mpyw/gormreuse/internal/goldentest"
)

// TestPanicInjection makes one stage of the analysis panic at a time, on
// testdata/src/panicguard, and checks that the panic is reported and leaves
// the conservative result (#315): the contract of a function whose check
// panicked is not trusted, a directive whose analysis panicked is not
// reported redundant, and a panic in the work that reads the whole package
// infers, exempts and exports nothing it could not prove.
//
// Every stage that the source runs under the guard has a case (#351). The
// distrust stage runs only for a function that depends on a distrusted one,
// so its case also makes the check of that one panic.
//
// It does not run in parallel: InjectGuardPanic sets what every analysis in
// the test binary runs.
func TestPanicInjection(t *testing.T) {
	testdata, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	const (
		pureFact     = "gormreuse pure finisher-only[0]"
		inferredFact = "gormreuse immutable-return"
		inputFact    = "gormreuse immutable-input(cb) finisher-only[0]"
		branchFact   = "gormreuse immutable-param finisher-only[0]"
		// What Each exports when its contract is not trusted (#347).
		eachQueries   = "gormreuse finisher-only[0]"
		reused        = "*gorm.DB reused"
		internalError = "gormreuse: internal error"
	)
	baseFacts := map[string]string{
		"Peek":    pureFact,
		"Conn":    inferredFact,
		"ConnVia": inferredFact,
		"Fresh":   inferredFact,
		"Each":    inputFact,
		"Branch":  branchFact,
	}
	with := func(changes map[string]string) map[string]string {
		facts := maps.Clone(baseFacts)
		for name, fact := range changes {
			if fact == "" {
				delete(facts, name)
			} else {
				facts[name] = fact
			}
		}
		return facts
	}
	tests := []struct {
		stage, fn string
		// seed, when set, is a function whose immutable-return check panics
		// too, so that the functions that depend on it are checked again in
		// the distrust stage.
		seed string
		// wantDiags are the substrings of the diagnostics, one each.
		wantDiags []string
		wantFacts map[string]string
	}{
		{"", "", "", nil, baseFacts},
		{"pure", "Peek", "", []string{internalError + " analyzing Peek", reused}, with(map[string]string{"Peek": "gormreuse pure pure-failed finisher-only[0]"})},
		{"immutable-return", "Conn", "", []string{internalError + " analyzing Conn"}, with(map[string]string{"Conn": "", "ConnVia": ""})},
		{"immutable-input", "Each", "", []string{internalError + " analyzing Each"}, with(map[string]string{"Each": eachQueries})},
		{"immutable-param", "Branch", "", []string{internalError + " analyzing Branch"}, baseFacts},
		{"reuse", "usePeek", "", []string{internalError + " analyzing usePeek"}, baseFacts},
		{"facts", "Conn", "", []string{internalError + " analyzing Conn"}, with(map[string]string{"Conn": ""})},
		{"distrust", "ConnVia", "Conn", []string{internalError + " analyzing Conn", internalError + " analyzing ConnVia"}, with(map[string]string{"Conn": "", "ConnVia": ""})},
		{"immutable-return inference", "", "", []string{internalError + " in immutable-return inference"}, with(map[string]string{"Fresh": "", "ConnVia": ""})},
		{"Scopes callbacks", "", "", []string{internalError + " in Scopes callbacks"}, baseFacts},
		{"immutable callbacks", "", "", []string{internalError + " in immutable callbacks", reused, reused}, baseFacts},
		{"immutable-input inference", "", "", []string{internalError + " in immutable-input inference", reused}, baseFacts},
		{"immutable-param inference", "", "", []string{internalError + " in immutable-param inference", reused}, baseFacts},
		{"Scopes warnings", "", "", []string{internalError + " in Scopes warnings"}, baseFacts},
		{"distrusted contracts", "", "", []string{internalError + " in distrusted contracts"}, with(map[string]string{"Conn": "", "ConnVia": "", "Fresh": "", "Each": eachQueries})},
		{"function variables", "", "", []string{internalError + " in function variables"}, baseFacts},
		{"unused immutable-input", "", "", []string{internalError + " in unused immutable-input"}, baseFacts},
		{"package initializer", "", "", []string{internalError + " in package initializer"}, baseFacts},
		{"directive reports", "", "", []string{internalError + " in directive reports"}, baseFacts},
		{"unused ignores", "", "", []string{internalError + " in unused ignores"}, baseFacts},
	}
	tested := make(map[string]bool)
	for _, tt := range tests {
		tested[tt.stage] = true
	}
	for _, stage := range guardedStages(t) {
		if !tested[stage] {
			t.Errorf("stage %q has no case", stage)
		}
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.stage, tt.fn), func(t *testing.T) {
			panics := map[string]string{tt.stage: tt.fn}
			if tt.seed != "" {
				panics["immutable-return"] = tt.seed
			}
			restore := internal.InjectGuardPanic(panics)
			defer restore()

			// The fixture has no expectations, so analysistest would report
			// every diagnostic as unexpected: they are checked below.
			results := analysistest.Run(goldentest.NoopT{}, testdata, gormreuse.Analyzer, "panicguard")
			var diags []string
			facts := make(map[string]string)
			for _, r := range results {
				for _, d := range r.Diagnostics {
					diags = append(diags, d.Message)
				}
				for obj, fs := range r.Facts {
					for _, f := range fs {
						facts[obj.Name()] = fmt.Sprint(f)
					}
				}
			}
			if len(diags) != len(tt.wantDiags) {
				t.Errorf("diagnostics = %q, want one matching each of %q", diags, tt.wantDiags)
			}
			for _, want := range tt.wantDiags {
				if !slices.ContainsFunc(diags, func(d string) bool { return strings.Contains(d, want) }) {
					t.Errorf("no diagnostic contains %q in %q", want, diags)
				}
			}
			for name, want := range tt.wantFacts {
				if facts[name] != want {
					t.Errorf("fact of %s = %q, want %q", name, facts[name], want)
				}
			}
			for name, got := range facts {
				if _, ok := tt.wantFacts[name]; !ok {
					t.Errorf("fact of %s = %q, want none", name, got)
				}
			}
		})
	}
}

// guardedStage matches the stage of a call of panicGuard.run or runPackage.
var guardedStage = regexp.MustCompile(`\.run(?:Package)?\("([^"]+)"`)

// guardedStages returns the stages that the files of this package run under
// the guard.
func guardedStages(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range guardedStage.FindAllStringSubmatch(string(src), -1) {
			stages = append(stages, m[1])
		}
	}
	if len(stages) == 0 {
		t.Fatal("found no guarded stage")
	}
	return stages
}
