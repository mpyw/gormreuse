// Package plugin registers gormreuse as a golangci-lint module plugin.
//
// Import it from .custom-gcl.yml to build a golangci-lint binary that holds
// gormreuse. The analyzer has no flags, so the plugin takes no settings, and
// any key under them is an error.
package plugin

import (
	"fmt"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"

	"github.com/mpyw/gormreuse"
)

func init() {
	register.Plugin(gormreuse.Analyzer.Name, newPlugin)
}

// pluginAnalyzer is the plugin. It holds no state, since there is nothing to
// configure.
type pluginAnalyzer struct{}

func newPlugin(settings any) (register.LinterPlugin, error) {
	if _, err := register.DecodeSettings[struct{}](settings); err != nil {
		return nil, fmt.Errorf("reading settings: %w", err)
	}
	return pluginAnalyzer{}, nil
}

// BuildAnalyzers gives gormreuse's analyzer. It reads no flags, so it is shared
// as it is.
func (pluginAnalyzer) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{gormreuse.Analyzer}, nil
}

// GetLoadMode asks for type information, which buildssa needs.
func (pluginAnalyzer) GetLoadMode() string {
	return register.LoadModeTypesInfo
}
