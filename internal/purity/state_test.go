package purity

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

// mockParam creates a mock parameter for testing.
// The name field is set directly for String() tests.
type mockParamWrapper struct {
	*ssa.Parameter
	name string
}

func (m *mockParamWrapper) Name() string {
	return m.name
}

// createMockParams creates mock parameters for testing.
// We use actual ssa.Parameter pointers for identity comparison.
var (
	mockP1 = &ssa.Parameter{}
	mockP2 = &ssa.Parameter{}
	mockP3 = &ssa.Parameter{}
)

func TestKind_String(t *testing.T) {
	tests := []struct {
		kind Kind
		want string
	}{
		{KindClean, "Clean"},
		{KindPolluted, "Polluted"},
		{KindDepends, "Depends"},
		{Kind(99), "Kind(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.kind.String(); got != tt.want {
				t.Errorf("Kind.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClean(t *testing.T) {
	s := Clean()
	if !s.IsClean() {
		t.Error("Clean() should return a Clean state")
	}
	if s.IsPolluted() {
		t.Error("Clean state should not be Polluted")
	}
	if s.IsDepends() {
		t.Error("Clean state should not be Depends")
	}
	if s.Kind() != KindClean {
		t.Errorf("Clean().Kind() = %v, want KindClean", s.Kind())
	}
	if s.Deps() != nil {
		t.Error("Clean state should have nil Deps")
	}
	if !s.IsValid() {
		t.Error("Clean state should be valid for pure function return")
	}
}

func TestPolluted(t *testing.T) {
	s := Polluted()
	if s.IsClean() {
		t.Error("Polluted state should not be Clean")
	}
	if !s.IsPolluted() {
		t.Error("Polluted() should return a Polluted state")
	}
	if s.IsDepends() {
		t.Error("Polluted state should not be Depends")
	}
	if s.Kind() != KindPolluted {
		t.Errorf("Polluted().Kind() = %v, want KindPolluted", s.Kind())
	}
	if s.Deps() != nil {
		t.Error("Polluted state should have nil Deps")
	}
	if s.IsValid() {
		t.Error("Polluted state should NOT be valid for pure function return")
	}
}

func TestDepends(t *testing.T) {
	p1 := mockP1
	p2 := mockP2

	t.Run("single param", func(t *testing.T) {
		s := Depends(p1)
		if s.IsClean() {
			t.Error("Depends state should not be Clean")
		}
		if s.IsPolluted() {
			t.Error("Depends state should not be Polluted")
		}
		if !s.IsDepends() {
			t.Error("Depends() should return a Depends state")
		}
		if s.Kind() != KindDepends {
			t.Errorf("Depends().Kind() = %v, want KindDepends", s.Kind())
		}
		if len(s.Deps()) != 1 {
			t.Errorf("Depends(p1).Deps() len = %d, want 1", len(s.Deps()))
		}
		if !s.IsValid() {
			t.Error("Depends state should be valid for pure function return")
		}
	})

	t.Run("multiple params", func(t *testing.T) {
		s := Depends(p1, p2)
		if len(s.Deps()) != 2 {
			t.Errorf("Depends(p1, p2).Deps() len = %d, want 2", len(s.Deps()))
		}
	})

	t.Run("deduplication", func(t *testing.T) {
		s := Depends(p1, p1, p1)
		if len(s.Deps()) != 1 {
			t.Errorf("Depends with duplicates should dedupe, got %d deps", len(s.Deps()))
		}
	})

	t.Run("empty becomes Clean", func(t *testing.T) {
		s := Depends()
		if !s.IsClean() {
			t.Error("Depends() with no params should become Clean")
		}
	})

	t.Run("nil params filtered", func(t *testing.T) {
		s := Depends(nil, p1, nil)
		if len(s.Deps()) != 1 {
			t.Errorf("Depends should filter nil params, got %d deps", len(s.Deps()))
		}
	})

	t.Run("all nil becomes Clean", func(t *testing.T) {
		s := Depends(nil, nil)
		if !s.IsClean() {
			t.Error("Depends with all nil should become Clean")
		}
	})
}

func TestState_DependsOn(t *testing.T) {
	p1 := mockP1
	p2 := mockP2

	t.Run("Clean never depends", func(t *testing.T) {
		if Clean().DependsOn(p1) {
			t.Error("Clean should not depend on any param")
		}
	})

	t.Run("Polluted never depends", func(t *testing.T) {
		if Polluted().DependsOn(p1) {
			t.Error("Polluted should not depend on any param")
		}
	})

	t.Run("Depends on included param", func(t *testing.T) {
		s := Depends(p1)
		if !s.DependsOn(p1) {
			t.Error("Should depend on included param")
		}
	})

	t.Run("Does not depend on excluded param", func(t *testing.T) {
		s := Depends(p1)
		if s.DependsOn(p2) {
			t.Error("Should not depend on excluded param")
		}
	})
}

func TestState_Merge(t *testing.T) {
	p1 := mockP1
	p2 := mockP2

	tests := []struct {
		name string
		a    State
		b    State
		want Kind
	}{
		{"Clean + Clean = Clean", Clean(), Clean(), KindClean},
		{"Clean + Polluted = Polluted", Clean(), Polluted(), KindPolluted},
		{"Polluted + Clean = Polluted", Polluted(), Clean(), KindPolluted},
		{"Polluted + Polluted = Polluted", Polluted(), Polluted(), KindPolluted},
		{"Clean + Depends = Depends", Clean(), Depends(p1), KindDepends},
		{"Depends + Clean = Depends", Depends(p1), Clean(), KindDepends},
		{"Depends + Depends = Depends", Depends(p1), Depends(p2), KindDepends},
		{"Depends + Polluted = Polluted", Depends(p1), Polluted(), KindPolluted},
		{"Polluted + Depends = Polluted", Polluted(), Depends(p1), KindPolluted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.a.Merge(tt.b)
			if result.Kind() != tt.want {
				t.Errorf("Merge() = %v, want %v", result.Kind(), tt.want)
			}
		})
	}

	t.Run("Depends merge combines params", func(t *testing.T) {
		s1 := Depends(p1)
		s2 := Depends(p2)
		result := s1.Merge(s2)
		if len(result.Deps()) != 2 {
			t.Errorf("Merged Depends should have 2 params, got %d", len(result.Deps()))
		}
	})

	t.Run("Depends merge deduplicates", func(t *testing.T) {
		s1 := Depends(p1)
		s2 := Depends(p1)
		result := s1.Merge(s2)
		if len(result.Deps()) != 1 {
			t.Errorf("Merged Depends should dedupe, got %d params", len(result.Deps()))
		}
	})
}

func TestState_String(t *testing.T) {
	t.Run("Clean", func(t *testing.T) {
		if got := Clean().String(); got != "Clean" {
			t.Errorf("Clean().String() = %q, want %q", got, "Clean")
		}
	})

	t.Run("Polluted", func(t *testing.T) {
		if got := Polluted().String(); got != "Polluted" {
			t.Errorf("Polluted().String() = %q, want %q", got, "Polluted")
		}
	})

	t.Run("Depends", func(t *testing.T) {
		s := Depends(mockP1)
		got := s.String()
		// Just check it starts with "Depends(" - param name may be empty
		if len(got) < 8 || got[:8] != "Depends(" {
			t.Errorf("Depends().String() = %q, should start with 'Depends('", got)
		}
	})
}

func TestState_Equal(t *testing.T) {
	p1 := mockP1
	p2 := mockP2

	tests := []struct {
		name string
		a    State
		b    State
		want bool
	}{
		{"Clean == Clean", Clean(), Clean(), true},
		{"Polluted == Polluted", Polluted(), Polluted(), true},
		{"Clean != Polluted", Clean(), Polluted(), false},
		{"Depends(p1) == Depends(p1)", Depends(p1), Depends(p1), true},
		{"Depends(p1) != Depends(p2)", Depends(p1), Depends(p2), false},
		{"Depends(p1,p2) == Depends(p1,p2)", Depends(p1, p2), Depends(p1, p2), true},
		{"Clean != Depends", Clean(), Depends(p1), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Errorf("Equal() = %v, want %v", got, tt.want)
			}
		})
	}
}
