package directive

import (
	"golang.org/x/tools/go/ssa"

	"github.com/mpyw/gormreuse/internal/ssa/origin"
)

// ParamRead is a value that an inferred immutable-return function returns
// from its parameter (#342): a field reached from it, such as r.db in
// `func (r *Repo) DB() *gorm.DB { return r.db }`, or the parameter itself
// when the function asserts it to *gorm.DB. Such a function is immutable only
// where the caller's argument holds an immutable value there, so each call
// reads the same selector of its own argument, as if it were written in place.
type ParamRead struct {
	// Param is the index of the parameter, the receiver first.
	Param int
	// Path holds the indexes of the fields selected from the parameter, as a
	// Go selector selects them: through a pointer, the field of what it
	// points to. It is empty for the parameter itself.
	Path []int
}

// InferParamReads adds fn to the set as Infer does, with the reads of its
// parameters that its results may be. A function with reads is immutable only
// at a call whose arguments hold immutable values there.
func (s *DirectiveFuncSet) InferParamReads(fn *ssa.Function, reads []ParamRead) {
	s.Infer(fn)
	if len(reads) == 0 {
		return
	}
	if s.paramReads == nil {
		s.paramReads = make(map[*ssa.Function][]ParamRead)
	}
	s.paramReads[fn] = reads
}

// ImportParamReads sets how the set finds the reads of a function of another
// package that its package inferred, from the fact it exported.
func (s *DirectiveFuncSet) ImportParamReads(imported func(fn *ssa.Function) []ParamRead) {
	if s != nil {
		s.importedParamReads = imported
	}
}

// ParamReads returns the reads of fn's parameters that its results may be:
// what InferParamReads added for a function of the package, or what the fact
// of a function of another package carries. It is nil for a function whose
// results are immutable at every call. fn may be a generic instance or a
// bound method wrapper (see origin.Declared). Neither s nor fn may be nil.
func (s *DirectiveFuncSet) ParamReads(fn *ssa.Function) []ParamRead {
	fn = origin.Declared(fn)
	if reads, ok := s.paramReads[fn]; ok {
		return reads
	}
	if s.importedParamReads != nil && fn.Syntax() == nil {
		return s.importedParamReads(fn)
	}
	return nil
}
