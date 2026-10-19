package directive

import (
	"go/ast"
	"go/token"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// =============================================================================
// Fixes That Delete a Directive (#248)
// =============================================================================

// RemovalTarget is one part of a directive comment that a fix deletes. Part is
// a name of the comment's list as written, such as "pure" or
// "immutable-input(cb)".
type RemovalTarget struct {
	Comment token.Pos // Position of the directive comment
	Part    string
}

// Removals reports the diagnostics whose fix deletes a part of a directive
// comment: an unused or redundant directive, or an unused ignore. It holds
// them back until Flush, because a comment can carry several such parts, each
// with its own diagnostic.
type Removals struct {
	pass     *analysis.Pass
	parts    map[token.Pos][]string // Parts to delete from each comment, over every diagnostic
	diags    []removalDiagnostic
	comments map[token.Pos]*ast.Comment // Built on the first Flush that needs it
}

// removalDiagnostic is one diagnostic recorded by Report.
type removalDiagnostic struct {
	pos     token.Pos
	message string
	targets []RemovalTarget
}

// NewRemovals returns an empty set that reports to pass.
func NewRemovals(pass *analysis.Pass) *Removals {
	return &Removals{pass: pass, parts: make(map[token.Pos][]string)}
}

// Report records a diagnostic at pos whose fix deletes targets. With no
// target the diagnostic has no fix. Flush reports it.
func (r *Removals) Report(pos token.Pos, message string, targets ...RemovalTarget) {
	r.diags = append(r.diags, removalDiagnostic{pos: pos, message: message, targets: targets})
	for _, t := range targets {
		r.parts[t.Comment] = append(r.parts[t.Comment], t.Part)
	}
}

// Flush reports every recorded diagnostic with its fix.
//
// A fix rewrites each comment it touches without the parts of every
// diagnostic recorded for that comment, not only its own. Two diagnostics on
// one comment, such as the unused pure and immutable-return of
// //gormreuse:pure,immutable-return on a function with no *gorm.DB, would
// otherwise rewrite the comment in two ways, and -fix could apply only one of
// them. Their fixes are now the same edit, which the drivers apply once.
func (r *Removals) Flush() {
	edits := make(map[token.Pos]*analysis.TextEdit)
	editOf := func(comment token.Pos) *analysis.TextEdit {
		edit, done := edits[comment]
		if !done {
			edit = r.removalEdit(comment, r.parts[comment])
			edits[comment] = edit
		}
		return edit
	}
	for _, d := range r.diags {
		diag := analysis.Diagnostic{Pos: d.pos, Message: d.message}
		var names []string
		var comments []token.Pos
		for _, t := range d.targets {
			names = append(names, "gormreuse:"+t.Part)
			if !slices.Contains(comments, t.Comment) {
				comments = append(comments, t.Comment)
			}
		}
		slices.Sort(comments)
		var textEdits []analysis.TextEdit
		for _, comment := range comments {
			edit := editOf(comment)
			if edit == nil {
				textEdits = nil
				break
			}
			textEdits = append(textEdits, *edit)
		}
		if len(textEdits) > 0 {
			diag.SuggestedFixes = []analysis.SuggestedFix{{Message: "Remove " + strings.Join(names, ", "), TextEdits: textEdits}}
		}
		r.pass.Report(diag)
	}
	r.diags = nil
}

// removalEdit returns the edit that deletes the parts remove from the
// directive comment at pos, or nil when it cannot be written:
//
//   - A part that others remain beside is cut out of the list.
//   - A comment left with no part keeps its reason, the text from the "//"
//     after the directive, which may be a note or another tool's directive
//     (//gormreuse:ignore //nolint:errcheck).
//   - A comment with no reason is deleted, with its whole line when nothing
//     else is on the line.
func (r *Removals) removalEdit(pos token.Pos, remove []string) *analysis.TextEdit {
	c := r.commentAt(pos)
	if c == nil {
		return nil
	}
	const prefix = "//" + directiveTool + ":"
	list, ok := strings.CutPrefix(c.Text, prefix)
	if !ok {
		return nil
	}
	head, _, hasReason := strings.Cut(list, "//")
	list = strings.TrimRight(head, " \t") // A valid directive has no argument
	parts := strings.Split(list, ",")
	for _, part := range remove {
		i := slices.Index(parts, part)
		if i < 0 {
			return nil
		}
		parts = slices.Delete(parts, i, i+1)
	}

	start := c.Pos() + token.Pos(len(prefix))
	switch {
	case len(parts) > 0:
		return &analysis.TextEdit{Pos: start, End: start + token.Pos(len(list)), NewText: []byte(strings.Join(parts, ","))}
	case hasReason:
		reasonStart := c.Pos() + token.Pos(len(prefix)+len(head))
		// The reason must not become a directive of its own once it starts
		// the comment, as in //gormreuse:ignore //gormreuse:pure.
		if directiveAddressed(c.Text[len(prefix)+len(head):]) {
			return nil
		}
		return &analysis.TextEdit{Pos: c.Pos(), End: reasonStart}
	}
	return r.commentDeletion(c)
}

// commentDeletion returns the edit that deletes comment c. A comment alone on
// its line goes with the line. A comment after code goes with the blanks
// before it.
func (r *Removals) commentDeletion(c *ast.Comment) *analysis.TextEdit {
	tf := r.pass.Fset.File(c.Pos())
	if tf == nil || r.pass.ReadFile == nil {
		return nil
	}
	src, err := r.pass.ReadFile(tf.Name())
	if err != nil || len(src) != tf.Size() {
		return nil
	}
	from, to := tf.Offset(c.Pos()), tf.Offset(c.End())
	for from > 0 && (src[from-1] == ' ' || src[from-1] == '\t') {
		from--
	}
	if from == 0 || src[from-1] == '\n' {
		// Alone on its line: delete the line, newline included.
		if i := strings.IndexByte(string(src[to:]), '\n'); i >= 0 {
			to += i + 1
		} else {
			to = len(src)
		}
	}
	return &analysis.TextEdit{Pos: tf.Pos(from), End: tf.Pos(to)}
}

// commentAt returns the comment of the pass's files at pos, or nil.
func (r *Removals) commentAt(pos token.Pos) *ast.Comment {
	if r.comments == nil {
		r.comments = make(map[token.Pos]*ast.Comment)
		for _, f := range r.pass.Files {
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					r.comments[c.Pos()] = c
				}
			}
		}
	}
	return r.comments[pos]
}
