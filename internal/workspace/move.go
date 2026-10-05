package workspace

import (
	"errors"
	"fmt"
	"strings"

	"sourcenav/internal/semantic"
	"sourcenav/internal/snapshot"
	"sourcenav/internal/syntax"
)

// ErrStaleMovePlan is returned by ApplyMove when a document touched by the
// plan changed after the plan was prepared. The caller must re-run
// PrepareMove against the current snapshot.
var ErrStaleMovePlan = errors.New("move plan is stale: a touched document changed after preview")

// ErrMoveUndoConflict is returned by UndoMove when a document touched by the
// last move changed after the move was applied, so restoring it would drop
// newer edits.
var ErrMoveUndoConflict = errors.New("cannot undo move: a touched document changed after the move")

// MoveFileEdit carries the fully rewritten text of one document touched by a
// move, bound to the document version it was computed from.
type MoveFileEdit struct {
	URI     snapshot.URI `json:"uri"`
	Version int          `json:"version"`
	Saved   bool         `json:"saved"`
	NewText string       `json:"newText"`
}

// MovePlan is the preview of a cross-file declaration migration. It is valid
// only against the per-document versions captured in Files.
type MovePlan struct {
	Version    VersionInfo    `json:"version"`
	SymbolID   uint64         `json:"symbolId"`
	SymbolName string         `json:"symbolName"`
	NewName    string         `json:"newName"`
	SourceURI  snapshot.URI   `json:"sourceUri"`
	DestURI    snapshot.URI   `json:"destUri"`
	KeepCompat bool           `json:"keepCompat"`
	Files      []MoveFileEdit `json:"files"`
}

// MoveResult reports an applied move. Every touched document becomes an
// unsaved session overlay (Saved=false).
type MoveResult struct {
	Version   VersionInfo    `json:"version"`
	SymbolID  uint64         `json:"symbolId"`
	NewName   string         `json:"newName"`
	Documents []DocumentInfo `json:"documents"`
}

// moveUndo records the pre-move state of every touched document plus the
// post-move versions, so undo can verify nothing else changed before
// restoring the original declaration, references and file states.
type moveUndo struct {
	pre     []snapshot.Document
	post    map[snapshot.URI]int
	srcMod  string
	destMod string
	oldName string
	newName string
}

// PrepareMove computes a preview of migrating the top-level declaration at
// uri/offset into the document destURI, optionally under a new name. When
// keepCompat is set, the source module keeps a compatible entry (a re-export
// under the original name) so references that are deliberately left in place
// still resolve to the moved symbol's identity. Nothing is modified; the
// plan must be confirmed with ApplyMove.
func (w *Workspace) PrepareMove(uri snapshot.URI, offset int, destURI snapshot.URI, newName string, keepCompat bool) (MovePlan, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if newName == "" {
		return MovePlan{}, fmt.Errorf("new name must not be empty")
	}
	if !syntax.IsIdent(newName) {
		return MovePlan{}, fmt.Errorf("invalid identifier %q", newName)
	}
	if destURI == uri {
		return MovePlan{}, fmt.Errorf("destination %q is the source file; use rename for in-place renames", destURI)
	}
	target, ok := w.findOccurrence(uri, offset)
	if !ok {
		if perr := w.parseErrs[uri]; perr != nil {
			return MovePlan{}, perr
		}
		return MovePlan{}, fmt.Errorf("no symbol at offset %d in %q", offset, uri)
	}
	if target.Role != semantic.RoleDecl || target.Symbol.LocalKey != "" {
		return MovePlan{}, fmt.Errorf("move target must be a top-level declaration, not a reference or local")
	}
	srcState := w.files[uri]
	if srcState == nil {
		return MovePlan{}, fmt.Errorf("source %q has no parsed state", uri)
	}
	destState := w.files[destURI]
	if destState == nil {
		if perr := w.parseErrs[destURI]; perr != nil {
			return MovePlan{}, perr
		}
		return MovePlan{}, fmt.Errorf("unknown or unparseable destination document %q", destURI)
	}

	oldName := target.Symbol.Name
	srcMod := syntax.ModulePath(srcState.tree)
	destMod := syntax.ModulePath(destState.tree)
	if srcMod == destMod && newName == oldName {
		return MovePlan{}, fmt.Errorf("declaration %q already lives in module %q under that name", oldName, printableMod(srcMod))
	}
	if _, exists := w.reg.GlobalExisting(destMod, newName); exists {
		return MovePlan{}, fmt.Errorf("name %q already denotes a symbol in module %q", newName, printableMod(destMod))
	}
	if keepCompat && srcMod != destMod && destMod == "" {
		return MovePlan{}, fmt.Errorf("cannot keep a compatible entry: destination module is anonymous and cannot be imported")
	}

	// Locate the declaration statement in the source tree.
	var stmtRange, nameRange syntax.Range
	found := false
	for _, s := range srcState.tree.Stmts {
		switch n := s.(type) {
		case *syntax.FnStmt:
			if n.NameRange.Start == target.Range.Start {
				stmtRange, nameRange, found = n.Range, n.NameRange, true
			}
		case *syntax.LetStmt:
			if n.NameRange.Start == target.Range.Start {
				stmtRange, nameRange, found = n.Range, n.NameRange, true
			}
		}
		if found {
			break
		}
	}
	if !found {
		return MovePlan{}, fmt.Errorf("declaration statement for %q not found in %q", oldName, uri)
	}

	srcDoc, _ := w.snapshot().Get(uri)
	destDoc, _ := w.snapshot().Get(destURI)

	// The migrated declaration text, with the name swapped in place.
	declText := srcDoc.Text[stmtRange.Start:stmtRange.End]
	rel := nameRange.Start - stmtRange.Start
	declText = declText[:rel] + newName + declText[rel+len(oldName):]

	// Source: cut the declaration statement (plus one adjacent newline).
	srcText := srcDoc.Text
	cutStart, cutEnd := stmtRange.Start, stmtRange.End
	switch {
	case cutEnd < len(srcText) && srcText[cutEnd] == '\n':
		cutEnd++
	case cutStart > 0 && srcText[cutStart-1] == '\n':
		cutStart--
	}
	srcText = srcText[:cutStart] + srcText[cutEnd:]

	// Source compat entry: re-export the moved symbol under its old name so
	// kept references (bare or module-qualified) still resolve to it.
	if keepCompat && (srcMod != destMod || newName != oldName) {
		if srcMod != destMod && syntax.Imports(srcState.tree)[pathBaseOf(destMod)] != destMod && !importsModule(srcState.tree, destMod) {
			importLine := `import "` + destMod + `"`
			if modStmt := findModuleStmt(srcState.tree); modStmt != nil {
				at := modStmt.Range.End
				srcText = srcText[:at] + "\n" + importLine + srcText[at:]
			} else {
				srcText = importLine + "\n" + srcText
			}
		}
		exportLine := "export " + newName
		if newName != oldName {
			exportLine += " as " + oldName
		}
		srcText = strings.TrimRight(srcText, "\n") + "\n" + exportLine + "\n"
	}

	// Destination: append the migrated declaration at the end.
	destText := destDoc.Text
	if strings.TrimSpace(destText) == "" {
		destText = declText + "\n"
	} else {
		destText = strings.TrimRight(destText, "\n") + "\n\n" + declText + "\n"
	}

	plan := MovePlan{
		Version:    w.versionInfoLocked(),
		SymbolID:   target.Symbol.ID,
		SymbolName: oldName,
		NewName:    newName,
		SourceURI:  uri,
		DestURI:    destURI,
		KeepCompat: keepCompat,
		Files: []MoveFileEdit{
			{URI: uri, Version: srcDoc.Version, Saved: srcDoc.Saved, NewText: srcText},
			{URI: destURI, Version: destDoc.Version, Saved: destDoc.Saved, NewText: destText},
		},
	}
	return plan, nil
}

func printableMod(mod string) string {
	if mod == "" {
		return "<anonymous>"
	}
	return mod
}

func pathBaseOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

func importsModule(t *syntax.File, mod string) bool {
	for _, s := range t.Stmts {
		if imp, ok := s.(*syntax.ImportStmt); ok && imp.Path == mod {
			return true
		}
	}
	return false
}

func findModuleStmt(t *syntax.File) *syntax.ModuleStmt {
	for _, s := range t.Stmts {
		if m, ok := s.(*syntax.ModuleStmt); ok {
			return m
		}
	}
	return nil
}

// ApplyMove commits a previously prepared move plan. Every touched document
// must still be at the previewed version; any mismatch fails with
// ErrStaleMovePlan and changes nothing. On success all documents are
// rewritten in one snapshot and the pre-move state is pushed onto the undo
// stack.
func (w *Workspace) ApplyMove(plan MovePlan) (MoveResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	undo := moveUndo{post: map[snapshot.URI]int{}}
	snap := w.snapshot()
	for _, fe := range plan.Files {
		doc, ok := snap.Get(fe.URI)
		if !ok {
			return MoveResult{}, fmt.Errorf("%w: %s no longer open", ErrStaleMovePlan, fe.URI)
		}
		if doc.Version != fe.Version {
			return MoveResult{}, fmt.Errorf("%w: %s at version %d, plan expected %d",
				ErrStaleMovePlan, fe.URI, doc.Version, fe.Version)
		}
		undo.pre = append(undo.pre, doc)
	}
	// Re-key the symbol identity before re-analysis so the migrated
	// declaration keeps the same Symbol ID under its new module/name.
	if st := w.files[plan.SourceURI]; st != nil {
		undo.srcMod = syntax.ModulePath(st.tree)
	}
	if st := w.files[plan.DestURI]; st != nil {
		undo.destMod = syntax.ModulePath(st.tree)
	}
	undo.oldName, undo.newName = plan.SymbolName, plan.NewName
	for _, fe := range plan.Files {
		doc, _ := snap.Get(fe.URI)
		doc.Text = fe.NewText
		doc.Version++
		doc.Saved = false
		snap = snap.WithDocument(w.nextSnap, doc)
		undo.post[fe.URI] = doc.Version
	}
	w.snap = snap
	w.nextSnap++
	w.reg.MoveKey(undo.srcMod, undo.oldName, undo.destMod, undo.newName)
	// A move is structural: rebind the whole project in one pass so the
	// compat entry and every kept reference resolve to the moved identity.
	w.rebuildAllLocked()
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

// UndoMove reverts the most recently applied move: the original declaration,
// its references and every touched file's state (text, version, saved flag)
// are restored exactly. It fails with ErrMoveUndoConflict when a touched
// document changed after the move.
func (w *Workspace) UndoMove() (StatusResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.moveUndo) == 0 {
		return StatusResult{}, fmt.Errorf("nothing to undo")
	}
	undo := w.moveUndo[len(w.moveUndo)-1]
	for _, pre := range undo.pre {
		doc, ok := w.snapshot().Get(pre.URI)
		if !ok {
			return StatusResult{}, fmt.Errorf("%w: %s no longer open", ErrMoveUndoConflict, pre.URI)
		}
		if doc.Version != undo.post[pre.URI] {
			return StatusResult{}, fmt.Errorf("%w: %s at version %d, move left it at %d",
				ErrMoveUndoConflict, pre.URI, doc.Version, undo.post[pre.URI])
		}
	}
	snap := w.snapshot()
	for _, pre := range undo.pre {
		snap = snap.WithDocument(w.nextSnap, pre)
	}
	w.snap = snap
	w.nextSnap++
	w.reg.MoveKey(undo.destMod, undo.newName, undo.srcMod, undo.oldName)
	w.rebuildAllLocked()
	w.moveUndo = w.moveUndo[:len(w.moveUndo)-1]
	return w.statusLocked(), nil
}
