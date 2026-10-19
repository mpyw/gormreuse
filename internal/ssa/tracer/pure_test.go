package tracer

import (
	"testing"

	"github.com/mpyw/gormreuse/internal/directive"
)

func TestIsPureFunction(t *testing.T) {
	t.Parallel()
	fixtures, all := loadFixtureProgram(t)
	// Syntax-backed pure set resolves //gormreuse:pure via each function's AST.
	tr := NewRootTracer(directive.NewPureFuncSet(nil, nil), nil, nil, nil, nil, nil, nil)

	if session := fixtureGormMethod(all, "Session"); session != nil && !tr.IsPureFunction(session) {
		t.Error("Session (immutable builtin) should count as pure")
	}
	if fn := fixtures["pureDoesNothing"]; fn == nil {
		t.Fatal("pureDoesNothing fixture missing")
	} else if !tr.IsPureFunction(fn) {
		t.Error("pureDoesNothing is marked //gormreuse:pure")
	}
	if fn := fixtures["nonPureHelper"]; fn != nil && tr.IsPureFunction(fn) {
		t.Error("nonPureHelper is not pure")
	}
	if tr.IsPureFunction(nil) {
		t.Error("nil is not pure")
	}
}
