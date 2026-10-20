package internal_test

import (
	"fmt"
	"maps"
	"path/filepath"
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
// It does not run in parallel: InjectGuardPanic sets what every analysis in
// the test binary runs.
func TestPanicInjection(t *testing.T) {
	testdata, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	const (
		pureFact      = "gormreuse pure"
		inferredFact  = "gormreuse immutable-return"
		inputFact     = "gormreuse immutable-input(cb)"
		branchFact    = "gormreuse immutable-param"
		reused        = "*gorm.DB reused"
		internalError = "gormreuse: internal error"
	)
	baseFacts := map[string]string{
		"Peek":   pureFact,
		"Conn":   inferredFact,
		"Fresh":  inferredFact,
		"Each":   inputFact,
		"Branch": branchFact,
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
		// wantDiags are the substrings of the diagnostics, one each.
		wantDiags []string
		wantFacts map[string]string
	}{
		{"", "", nil, baseFacts},
		{"pure", "Peek", []string{internalError + " analyzing Peek", reused}, with(map[string]string{"Peek": "gormreuse pure pure-failed"})},
		{"immutable-return", "Conn", []string{internalError + " analyzing Conn"}, with(map[string]string{"Conn": ""})},
		{"immutable-input", "Each", []string{internalError + " analyzing Each"}, with(map[string]string{"Each": ""})},
		{"immutable-param", "Branch", []string{internalError + " analyzing Branch"}, baseFacts},
		{"immutable-return inference", "", []string{internalError + " in immutable-return inference"}, with(map[string]string{"Fresh": ""})},
		{"Scopes callbacks", "", []string{internalError + " in Scopes callbacks"}, baseFacts},
		{"immutable callbacks", "", []string{internalError + " in immutable callbacks", reused}, baseFacts},
		{"Scopes warnings", "", []string{internalError + " in Scopes warnings"}, baseFacts},
		{"distrusted contracts", "", []string{internalError + " in distrusted contracts"}, with(map[string]string{"Conn": "", "Fresh": "", "Each": ""})},
		{"function variables", "", []string{internalError + " in function variables"}, baseFacts},
		{"unused immutable-input", "", []string{internalError + " in unused immutable-input"}, baseFacts},
		{"directive reports", "", []string{internalError + " in directive reports"}, baseFacts},
		{"unused ignores", "", []string{internalError + " in unused ignores"}, baseFacts},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.stage, tt.fn), func(t *testing.T) {
			restore := internal.InjectGuardPanic(tt.stage, tt.fn)
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
