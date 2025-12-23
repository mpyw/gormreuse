package purity

import (
	"testing"

	"golang.org/x/tools/go/ssa"
)

// createMockParams creates mock parameters for testing.
// We use actual ssa.Parameter pointers for identity comparison.
var (
	mockP1 = &ssa.Parameter{}
	mockP2 = &ssa.Parameter{}
)

func TestClean(t *testing.T) {
	s := Clean()
	if s.IsPolluted() {
		t.Error("Clean state should not be Polluted")
	}
	if s.IsDepends() {
		t.Error("Clean state should not be Depends")
	}
	if s.Deps() != nil {
		t.Error("Clean state should have nil Deps")
	}
}

func TestPolluted(t *testing.T) {
	s := Polluted()
	if !s.IsPolluted() {
		t.Error("Polluted() should return a Polluted state")
	}
	if s.IsDepends() {
		t.Error("Polluted state should not be Depends")
	}
	if s.Deps() != nil {
		t.Error("Polluted state should have nil Deps")
	}
}

func TestDepends(t *testing.T) {
	p1 := mockP1
	p2 := mockP2

	t.Run("single param", func(t *testing.T) {
		s := Depends(p1)
		if s.IsPolluted() {
			t.Error("Depends state should not be Polluted")
		}
		if !s.IsDepends() {
			t.Error("Depends() should return a Depends state")
		}
		if len(s.Deps()) != 1 {
			t.Errorf("Depends(p1).Deps() len = %d, want 1", len(s.Deps()))
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
		if s.IsDepends() || s.IsPolluted() {
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
		if s.IsDepends() || s.IsPolluted() {
			t.Error("Depends with all nil should become Clean")
		}
	})
}

func TestState_Merge(t *testing.T) {
	p1 := mockP1
	p2 := mockP2

	t.Run("Clean + Clean = Clean", func(t *testing.T) {
		result := Clean().Merge(Clean())
		if result.IsPolluted() || result.IsDepends() {
			t.Error("Clean + Clean should be Clean")
		}
	})

	t.Run("Clean + Polluted = Polluted", func(t *testing.T) {
		result := Clean().Merge(Polluted())
		if !result.IsPolluted() {
			t.Error("Clean + Polluted should be Polluted")
		}
	})

	t.Run("Polluted + Clean = Polluted", func(t *testing.T) {
		result := Polluted().Merge(Clean())
		if !result.IsPolluted() {
			t.Error("Polluted + Clean should be Polluted")
		}
	})

	t.Run("Clean + Depends = Depends", func(t *testing.T) {
		result := Clean().Merge(Depends(p1))
		if !result.IsDepends() {
			t.Error("Clean + Depends should be Depends")
		}
	})

	t.Run("Depends + Polluted = Polluted", func(t *testing.T) {
		result := Depends(p1).Merge(Polluted())
		if !result.IsPolluted() {
			t.Error("Depends + Polluted should be Polluted")
		}
	})

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
