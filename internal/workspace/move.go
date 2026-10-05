// Declaration migration: move a top-level declaration to another file,
// optionally renaming it in the same atomic step. References are either
// rewritten to the new home or deliberately retained; retained references
// keep resolving because the source file gains a compatibility entry
// (import + re-export alias) that binds the old qualified name to the moved
// symbol identity. Every applied move is undoable: UndoMove restores the
// exact prior declaration, references and document states.
package workspace

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"sourcenav/internal/semantic"
	"sourcenav/internal/snapshot"
	"sourcenav/internal/syntax"
)

// ErrStaleMovePlan is returned by ApplyMove when a document touched by the
// plan changed after the plan was prepared.
var ErrStaleMovePlan = errors.New("move plan is stale: a touched document changed after preview")

// MoveFileEdit is the full-text rewrite of one document, bound to the exact
// version and bytes it was computed from.
type MoveFileEdit struct {
	URI     snapshot.URI `json:"uri"`
	Version int          `json:"version"`
	Saved   bool         `json:"saved"`
	OldText string       `json:"oldText"`
	NewText string       `json:"newText"`
}

// MovePlan previews moving the declaration at the source position into
// TargetURI under NewName. KeepURIs lists documents whose references must be
// retained verbatim; they keep resolving through a compatibility entry added
// to the source file. All other references are rewritten to the new home.
type MovePlan struct {
	Version    VersionInfo    `json:"version"`
	SymbolID   uint64         `json:"symbolId"`
	SymbolName string         `json:"symbolName"`
	NewName    string         `json:"newName"`
	SourceURI  snapshot.URI   `json:"sourceUri"`
	TargetURI  snapshot.URI   `json:"targetUri"`
	KeepURIs   []snapshot.URI `json:"keepUris,omitempty"`
	Files      []MoveFileEdit `json:"files"`
}

// MoveResult reports an applied move.
type MoveResult struct {
	Version   VersionInfo    `json:"version"`
	SymbolID  uint64         `json:"symbolId"`
	NewName   string         `json:"newName"`
	Documents []DocumentInfo `json:"documents"`
}

// moveUndo records the exact document states around one applied move so
// UndoMove can restore them byte-for-byte.
type moveUndo struct {
	before []snapshot.Document
	after  []snapshot.Document
}

// PrepareMove computes a migration preview without changing any text. The
// symbol at uri/offset must be a top-level fn/let declaration. newName may be
// empty to keep the current name.
func (w *Workspace) PrepareMove(uri snapshot.URI, offset int, targetURI snapshot.URI, newName string, keep []snapshot.URI) (MovePlan, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	target, ok := w.findOccurrence(uri, offset)
	if !ok {
		if perr := w.parseErrs[uri]; perr != nil {
			return MovePlan{}, perr
		}
		return MovePlan{}, fmt.Errorf("no symbol at offset %d in %q", offset, uri)
	}
	if target.Role != semantic.RoleDecl || target.Symbol.LocalKey != "" {
		return MovePlan{}, fmt.Errorf("only a top-level declaration can be moved")
	}
	if targetURI == uri {
		return MovePlan{}, fmt.Errorf("target file must differ from source %s", uri)
	}
	srcState := w.files[uri]
	dstState := w.files[targetURI]
	if srcState == nil || dstState == nil {
		return MovePlan{}, fmt.Errorf("source and target files must both be open and parseable")
	}
	if perr := w.parseErrs[targetURI]; perr != nil {
		return MovePlan{}, perr
	}

	// Locate the declaration statement in the source tree.
	var declStmt syntax.Stmt
	var declNameRange syntax.Range
	for _, st := range srcState.tree.Stmts {
		switch n := st.(type) {
		case *syntax.FnStmt:
			if n.NameRange == target.Range {
				declStmt, declNameRange = n, n.NameRange
			}
		case *syntax.LetStmt:
			if n.NameRange == target.Range {
				declStmt, declNameRange = n, n.NameRange
			}
		}
	}
	if declStmt == nil {
		return MovePlan{}, fmt.Errorf("declaration statement not found in %s", uri)
	}

	oldName := target.Symbol.Name
	if newName == "" {
		newName = oldName
	}
	if !syntax.IsIdent(newName) {
		return MovePlan{}, fmt.Errorf("invalid identifier %q", newName)
	}

	srcMod := moduleOf(srcState.tree)
	dstMod := moduleOf(dstState.tree)
	if existing, found := w.reg.GlobalExisting(dstMod, newName); found && existing.ID != target.Symbol.ID {
		return MovePlan{}, fmt.Errorf("name %q already denotes a symbol in module %q", newName, dstMod)
	}

	srcDoc, _ := w.snapshot().Get(uri)
	dstDoc, _ := w.snapshot().Get(targetURI)

	// All edits are computed against the original document bytes and applied
	// in one pass, so the declaration cut, reference rewrites and import
	// insertions can never shift each other's coordinates.
	editsByFile := map[snapshot.URI][]textEdit{}
	addEdit := func(u snapshot.URI, e textEdit) {
		editsByFile[u] = append(editsByFile[u], e)
	}

	// 1. Source: cut the declaration statement (plus one adjacent newline).
	stmtRange := nodeRangeOf(declStmt)
	start, end := stmtRange.Start, stmtRange.End
	if end < len(srcDoc.Text) && srcDoc.Text[end] == '\n' {
		end++
	} else if start > 0 && srcDoc.Text[start-1] == '\n' {
		start--
	}
	addEdit(uri, textEdit{start: start, end: end, repl: ""})

	// 2. References: rewrite every use outside the keep set to the new home.
	// Uses inside the moved declaration itself (recursion) travel with the
	// declaration text instead.
	kept := map[snapshot.URI]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	keptRefs := 0
	importAdded := map[snapshot.URI]bool{}
	var declTextEdits []textEdit
	for _, u := range w.snapshot().URIs() {
		st := w.files[u]
		if st == nil {
			continue
		}
		var uses []semantic.Occurrence
		for _, occ := range st.analysis.Occurrences {
			if occ.Symbol.ID == target.Symbol.ID && occ.Role == semantic.RoleUse {
				uses = append(uses, occ)
			}
		}
		if len(uses) == 0 {
			continue
		}
		if kept[u] {
			keptRefs += len(uses)
			continue
		}
		fileMod := moduleOf(st.tree)
		qual, needImport := qualifierFor(importQualifiers(st.tree), dstMod)
		for _, occ := range uses {
			if u == uri && occ.Range.Start >= stmtRange.Start && occ.Range.End <= stmtRange.End {
				// Recursive use inside the moved declaration: rename it
				// within the carried declaration text.
				declTextEdits = append(declTextEdits, textEdit{
					start: occ.Range.Start - stmtRange.Start,
					end:   occ.Range.End - stmtRange.Start,
					repl:  newName,
				})
				continue
			}
			spanStart := occ.Range.Start
			if occ.Qual != nil {
				spanStart = occ.Qual.Start
			}
			var repl string
			switch {
			case fileMod == dstMod:
				repl = newName
			case dstMod == "":
				return MovePlan{}, fmt.Errorf(
					"cannot rewrite reference in %s: target module is anonymous", u)
			default:
				repl = qual + "." + newName
			}
			addEdit(u, textEdit{start: spanStart, end: occ.Range.End, repl: repl})
		}
		if needImport && fileMod != dstMod && dstMod != "" {
			addEdit(u, importEdit(st.tree, dstMod))
			importAdded[u] = true
		}
	}

	// 3. The moved declaration text, with its own name and any recursive
	// uses renamed, is appended to the target file.
	declText := srcDoc.Text[stmtRange.Start:stmtRange.End]
	{
		rel := declNameRange.Start - stmtRange.Start
		declTextEdits = append(declTextEdits, textEdit{
			start: rel, end: rel + len(oldName), repl: newName,
		})
		declText = applyTextEdits(declText, declTextEdits)
	}
	newTexts := map[snapshot.URI]string{}
	for u, edits := range editsByFile {
		doc, _ := w.snapshot().Get(u)
		newTexts[u] = applyTextEdits(doc.Text, edits)
	}
	{
		dstNew := dstDoc.Text
		if !strings.HasSuffix(dstNew, "\n") {
			dstNew += "\n"
		}
		dstNew += "\n" + declText + "\n"
		newTexts[targetURI] = dstNew
	}

	// 4. Compatibility entry: retained references still name the old
	// module-qualified (or bare) name, so the source file re-exports the
	// moved symbol under the old name.
	if keptRefs > 0 {
		if srcMod == "" {
			return MovePlan{}, fmt.Errorf(
				"cannot retain references in %d document(s): source file %s has no module directive to host a compatibility entry",
				keptRefs, uri)
		}
		compat := ""
		if !importAdded[uri] {
			if _, need := qualifierFor(importQualifiers(srcState.tree), dstMod); need && srcMod != dstMod {
				compat += "import " + strconv.Quote(dstMod) + "\n"
			}
		}
		if newName == oldName {
			compat += "export " + newName + "\n"
		} else {
			compat += "export " + newName + " as " + oldName + "\n"
		}
		src := newTexts[uri]
		if !strings.HasSuffix(src, "\n") {
			src += "\n"
		}
		newTexts[uri] = src + compat
	}

	plan := MovePlan{
		Version:    w.versionInfoLocked(),
		SymbolID:   target.Symbol.ID,
		SymbolName: oldName,
		NewName:    newName,
		SourceURI:  uri,
		TargetURI:  targetURI,
		KeepURIs:   append([]snapshot.URI(nil), keep...),
	}
	for u, text := range newTexts {
		doc, _ := w.snapshot().Get(u)
		if text == doc.Text {
			continue
		}
		plan.Files = append(plan.Files, MoveFileEdit{
			URI: u, Version: doc.Version, Saved: doc.Saved,
			OldText: doc.Text, NewText: text,
		})
	}
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].URI < plan.Files[j].URI })
	return plan, nil
}

// textEdit is one half-open byte-range replacement.
type textEdit struct {
	start, end int
	repl       string
}

func applyTextEdits(text string, edits []textEdit) string {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	last := len(text) + 1
	for _, e := range edits {
		if e.start < 0 || e.end < e.start || e.end > len(text) || e.end > last {
			continue
		}
		text = text[:e.start] + e.repl + text[e.end:]
		last = e.start
	}
	return text
}

// importQualifiers maps module path to the qualifier a file already imports
// it under.
func importQualifiers(t *syntax.File) map[string]string {
	m := map[string]string{}
	for _, s := range t.Stmts {
		imp, ok := s.(*syntax.ImportStmt)
		if !ok {
			continue
		}
		qual := imp.Path
		if i := strings.LastIndex(qual, "."); i >= 0 {
			qual = qual[i+1:]
		}
		if imp.HasAlias {
			qual = imp.Alias
		}
		m[imp.Path] = qual
	}
	return m
}

// qualifierFor returns the qualifier to address a module from a file, and
// whether an import directive must be added.
func qualifierFor(imports map[string]string, mod string) (string, bool) {
	if q, ok := imports[mod]; ok {
		return q, false
	}
	base := mod
	if i := strings.LastIndex(base, "."); i >= 0 {
		base = base[i+1:]
	}
	return base, true
}

// importEdit inserts an import directive after the module directive (or at
// the top of the file).
func importEdit(t *syntax.File, mod string) textEdit {
	pos := 0
	for _, s := range t.Stmts {
		if m, ok := s.(*syntax.ModuleStmt); ok {
			pos = nodeRangeOf(m).End
			break
		}
	}
	return textEdit{start: pos, end: pos, repl: "import " + strconv.Quote(mod) + "\n"}
}

func nodeRangeOf(st syntax.Stmt) syntax.Range {
	switch n := st.(type) {
	case *syntax.FnStmt:
		return n.Range
	case *syntax.LetStmt:
		return n.Range
	case *syntax.ModuleStmt:
		return n.Range
	case *syntax.ImportStmt:
		return n.Range
	case *syntax.ExportStmt:
		return n.Range
	}
	return syntax.Range{}
}

// ApplyMove commits a prepared plan atomically after validating that every
// touched document still matches the previewed version and bytes. The prior
// state is pushed on the undo stack; UndoMove restores it exactly.
func (w *Workspace) ApplyMove(plan MovePlan) (MoveResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	undo := moveUndo{}
	snap := w.snapshot()
	for _, fe := range plan.Files {
		doc, ok := snap.Get(fe.URI)
		if !ok {
			return MoveResult{}, fmt.Errorf("%w: %s no longer open", ErrStaleMovePlan, fe.URI)
		}
		if doc.Version != fe.Version || doc.Text != fe.OldText {
			return MoveResult{}, fmt.Errorf("%w: %s changed after preview",
				ErrStaleMovePlan, fe.URI)
		}
		undo.before = append(undo.before, doc)
	}
	for _, fe := range plan.Files {
		doc, _ := snap.Get(fe.URI)
		doc.Text = fe.NewText
		doc.Version++
		doc.Saved = false
		snap = snap.WithDocument(w.nextSnap, doc)
		undo.after = append(undo.after, doc)
	}
	w.snap = snap
	w.nextSnap++
	for _, fe := range plan.Files {
		w.rebuild(fe.URI)
	}
	w.reanalyzeAll()
	w.moveUndo = append(w.moveUndo, undo)

	res := MoveResult{Version: w.versionInfoLocked(), SymbolID: plan.SymbolID, NewName: plan.NewName}
	for _, u := range w.snapshot().URIs() {
		doc, _ := w.snapshot().Get(u)
		res.Documents = append(res.Documents, DocumentInfo{
			URI: u, Version: doc.Version, Saved: doc.Saved, Bytes: len(doc.Text),
		})
	}
	return res, nil
}

// UndoMove reverts the most recently applied move, restoring the exact prior
// declaration, references and per-document version/saved state. It refuses to
// clobber documents that changed after the move was applied.
func (w *Workspace) UndoMove() (VersionInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := len(w.moveUndo)
	if n == 0 {
		return VersionInfo{}, fmt.Errorf("nothing to undo")
	}
	undo := w.moveUndo[n-1]
	snap := w.snapshot()
	for _, after := range undo.after {
		doc, ok := snap.Get(after.URI)
		if !ok {
			return VersionInfo{}, fmt.Errorf("cannot undo: %s no longer open", after.URI)
		}
		if doc.Text != after.Text || doc.Version != after.Version {
			return VersionInfo{}, fmt.Errorf(
				"cannot undo: %s changed after the move was applied", after.URI)
		}
	}
	w.moveUndo = w.moveUndo[:n-1]
	for _, before := range undo.before {
		snap = snap.WithDocument(w.nextSnap, before)
	}
	w.snap = snap
	w.nextSnap++
	for _, before := range undo.before {
		w.rebuild(before.URI)
	}
	w.reanalyzeAll()
	return w.versionInfoLocked(), nil
}
