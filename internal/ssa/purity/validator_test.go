package purity_test

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/directive"
	"github.com/mpyw/gormreuse/internal/ssa/purity"
	"github.com/mpyw/gormreuse/internal/ssa/tracer"
)

func TestValidatorValidate(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureFuncs(t)
	// A syntax-backed pure set: Contains resolves //gormreuse:pure via each
	// function's AST, which is enough for the fixtures.
	pureFuncs := directive.NewPureFuncSet(nil, nil)

	tests := []struct {
		fn      string
		want    []string // substrings that must each appear in some violation
		wantDef bool     // at least one violation is Definitive
		clean   bool     // expect zero violations
	}{
		{fn: "purePollutesByWhere", want: []string{"pollutes", "Where"}, wantDef: true},
		{fn: "purePollutesByFind", want: []string{"Find"}, wantDef: true},
		{fn: "purePollutesByChain", want: []string{"Where", "Find"}, wantDef: true},
		{fn: "pureReturnsWhereResult", want: []string{"Where"}, wantDef: true},
		{fn: "purePollutesOneOfMany", want: []string{"Where"}, wantDef: true},
		{fn: "pureReturnsNonPureFuncResult", want: []string{"non-pure function", "nonPureHelperReturns"}},
		{fn: "pureLeaksViaChanSend", want: []string{"channel send"}, wantDef: true},
		{fn: "pureLeaksViaSliceStore", want: []string{"slice/array store"}, wantDef: true},
		{fn: "pureLeaksViaMapStore", want: []string{"map store"}, wantDef: true},
		{fn: "pureLeaksViaInterfaceArg", want: []string{"non-pure function", "nonPureTakesAny"}},
		{fn: "pcGo", want: []string{"Find"}, wantDef: true},
		{fn: "pcGoHelper", want: []string{"non-pure function", "pcSink"}},
		{fn: "pcStoreGlobal", want: []string{"package-level variable"}, wantDef: true},
		{fn: "pcStoreThroughPointer", want: []string{"through a pointer"}, wantDef: true},
		{fn: "pcIIFE", want: []string{"Find"}, wantDef: true},
		{fn: "pcBound", want: []string{"method value Find"}, wantDef: true},
		{fn: "pcStructParam", want: []string{"Find"}, wantDef: true},
		{fn: "pcLogs", clean: true},
		{fn: "pcSessionWhere", clean: true},
		{fn: "pcStructParamLen", clean: true},
		{fn: "pureDoesNothing", clean: true},
		{fn: "pureOnlyCallsSession", clean: true},
		{fn: "purePassesToPure", clean: true},
	}

	for _, tc := range tests {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			fn, ok := funcs[tc.fn]
			if !ok {
				t.Fatalf("fixture function %q not found", tc.fn)
			}
			violations := purity.NewValidator(fn, pureFuncs, nil, nil, nil).Validate()

			if tc.clean {
				if len(violations) != 0 {
					t.Errorf("%s: expected no violations, got %d: %+v", tc.fn, len(violations), violations)
				}
				return
			}

			if len(violations) == 0 {
				t.Fatalf("%s: expected violations, got none", tc.fn)
			}
			joined := ""
			leak := false
			for _, v := range violations {
				joined += v.Message + "\n"
				if v.Definitive {
					leak = true
				}
			}
			for _, sub := range tc.want {
				if !strings.Contains(joined, sub) {
					t.Errorf("%s: violations missing %q; got:\n%s", tc.fn, sub, joined)
				}
			}
			if tc.wantDef && !leak {
				t.Errorf("%s: expected a definitive violation, got none", tc.fn)
			}
			if !tc.wantDef && leak {
				t.Errorf("%s: unexpected definitive violation (should be conservative)", tc.fn)
			}
		})
	}
}

// TestValidatorValidateTransitive checks that passing an argument to a pure
// function that already failed validation is itself a definitive violation
// (issue #187).
func TestValidatorValidateTransitive(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureFuncs(t)
	pureFuncs := directive.NewPureFuncSet(nil, nil)
	wrapper := funcs["pcWrapper"]

	if v := purity.NewValidator(wrapper, pureFuncs, nil, nil, nil).Validate(); len(v) != 0 {
		t.Fatalf("pcWrapper with trusted pcBroken: expected no violations, got %+v", v)
	}

	failed := &tracer.PureVerdicts{Failed: map[*ssa.Function]bool{funcs["pcBroken"]: true}}
	v := purity.NewValidator(wrapper, pureFuncs, nil, failed, nil).Validate()
	if len(v) != 1 || !v[0].Definitive || !strings.Contains(v[0].Message, "pcBroken, which violates its pure contract") {
		t.Fatalf("pcWrapper with failed pcBroken: expected one definitive violation, got %+v", v)
	}
}

// TestValidatorReturnedParams checks which parameters a pure function's
// result may be (issue #188).
func TestValidatorReturnedParams(t *testing.T) {
	t.Parallel()
	funcs := loadFixtureFuncs(t)
	pureFuncs := directive.NewPureFuncSet(nil, nil)

	tests := []struct {
		fn       string
		verdicts *tracer.PureVerdicts
		want     []int
	}{
		{fn: "paReturn", want: []int{0}},
		{fn: "paMaybe", want: []int{0}},
		{fn: "paThroughHolder", want: []int{0}},
		{fn: "paThroughIIFE", want: []int{0}},
		{fn: "paDeref", want: []int{0}},
		{fn: "paFirst", want: []int{0}},
		{fn: "paTenant", want: nil},
		{fn: "paIIFEFresh", want: nil},
		// A wrapper returns its argument only once the callee is known to.
		{fn: "paWrapper", want: nil},
		{fn: "paWrapper", verdicts: &tracer.PureVerdicts{Returned: map[*ssa.Function][]int{funcs["paReturn"]: {0}}}, want: []int{0}},
	}
	for _, tc := range tests {
		got := purity.NewValidator(funcs[tc.fn], pureFuncs, nil, tc.verdicts, nil).ReturnedParams()
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: ReturnedParams() = %v, want %v", tc.fn, got, tc.want)
		}
	}

	if got := purity.NewValidator(nil, pureFuncs, nil, nil, nil).ReturnedParams(); got != nil {
		t.Errorf("nil function: expected nil, got %v", got)
	}
}

// TestValidatorValidateNil covers the nil/blockless guards.
func TestValidatorValidateNil(t *testing.T) {
	t.Parallel()
	if v := purity.NewValidator(nil, directive.NewPureFuncSet(nil, nil), nil, nil, nil).Validate(); v != nil {
		t.Errorf("nil function: expected nil, got %+v", v)
	}
	if v := purity.NewValidator(&ssa.Function{}, directive.NewPureFuncSet(nil, nil), nil, nil, nil).Validate(); v != nil {
		t.Errorf("blockless function: expected nil, got %+v", v)
	}
}
