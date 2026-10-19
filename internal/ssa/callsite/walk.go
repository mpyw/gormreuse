package callsite

import "golang.org/x/tools/go/ssa"

// WalkInstrs visits every instruction in funcs and their nested anonymous
// functions, visiting each function exactly once.
func WalkInstrs(funcs []*ssa.Function, visit func(ssa.Instruction)) {
	visited := make(map[*ssa.Function]bool)
	var rec func(fn *ssa.Function)
	rec = func(fn *ssa.Function) {
		if fn == nil || visited[fn] {
			return
		}
		visited[fn] = true
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				visit(instr)
			}
		}
		for _, af := range fn.AnonFuncs {
			rec(af)
		}
	}
	for _, fn := range funcs {
		rec(fn)
	}
}
