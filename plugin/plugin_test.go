package plugin_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/mpyw/gormreuse"
	_ "github.com/mpyw/gormreuse/plugin"
)

func TestPluginReports(t *testing.T) {
	p, err := newPlugin(t)(nil)
	if err != nil {
		t.Fatal(err)
	}
	as, err := p.BuildAnalyzers()
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0] != gormreuse.Analyzer {
		t.Fatalf("got analyzers %v, want only gormreuse.Analyzer", as)
	}
	testdata, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, testdata, as[0], "gormreuse")
}

func TestPluginAcceptsEmptySettings(t *testing.T) {
	if _, err := newPlugin(t)(map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestPluginRejectsSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings any
	}{
		{"a key", map[string]any{"test": false}},
		{"settings that are not a map", []any{"test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newPlugin(t)(tt.settings)
			if err == nil || !strings.Contains(err.Error(), "decoding settings") {
				t.Fatalf("got error %v, want a decoding error", err)
			}
		})
	}
}

func TestPluginLoadMode(t *testing.T) {
	p, err := newPlugin(t)(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.GetLoadMode(); got != register.LoadModeTypesInfo {
		t.Errorf("load mode %q, want %q", got, register.LoadModeTypesInfo)
	}
}

// newPlugin finds the constructor the package registered.
func newPlugin(t *testing.T) register.NewPlugin {
	t.Helper()
	np, err := register.GetPlugin("gormreuse")
	if err != nil {
		t.Fatal(err)
	}
	return np
}
