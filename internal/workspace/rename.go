package workspace

import (
	"errors"
	"fmt"
	"sort"

	"sourcenav/internal/semantic"
	"sourcenav/internal/snapshot"
	"sourcenav/internal/syntax"
)

// ErrStaleRenamePlan is returned by ApplyRename when any document touched by
// the plan changed after the plan was prepared. The caller must re-run
// PrepareRename against the current snapshot instead of applying old offsets.
var ErrStaleRenamePlan = errors.New("rename plan is stale: a touched document changed after preview")

// RenameEdit is one identifier replacement inside a document. Text records
// the source bytes the range covered at preview time, so ApplyRename can
// verify the plan still matches reality before writing anything.
type RenameEdit struct {
	Range   Range  `json:"range"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// RenameFileEdit groups the edits of one document, bound to the document
// version they were computed from.
type RenameFileEdit struct {
	URI     snapshot.URI `json:"uri"`
	Version int          `json:"version"`
	Saved   bool         `json:"saved"`
	Edits   []RenameEdit `json:"edits"`
}

// RenamePlan is the preview of a cross-file rename. It is valid only against
// the snapshot and per-document versions captured in Version.
type RenamePlan struct {
	Version    VersionInfo      `json:"version"`
	SymbolID   uint64           `json:"symbolId"`
	SymbolName string           `json:"symbolName"`
	NewName    string           `json:"newName"`
	Files      []RenameFileEdit `json:"files"`
}

// RenameResult reports an applied rename. Every touched document becomes an
// unsaved session overlay (Saved=false); persisting to disk is a separate,
// explicit Save step so session text and on-disk bytes stay distinguishable.
type RenameResult struct {
	Version   VersionInfo    `json:"version"`
	SymbolID  uint64         `json:"symbolId"`
	NewName   string         `json:"newName"`
	Documents []DocumentInfo `json:"documents"`
}

// PrepareRename computes a preview of renaming the symbol at uri/offset to
// newName. Only occurrences bound to that exact symbol identity are included:
// shadowing locals and unrelated same-named declarations are never touched.
// Nothing is modified; the plan must be confirmed with ApplyRename.
func (w *Workspace) PrepareRename(uri snapshot.URI, offset int, newName string) (RenamePlan, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !syntax.IsIdent(newName) {
		return RenamePlan{}, fmt.Errorf("invalid identifier %q", newName)
	}
	target, ok := w.findOccurrence(uri, offset)
	if !ok {
		if perr := w.parseErrs[uri]; perr != nil {
			return RenamePlan{}, perr
		}
		return RenamePlan{}, fmt.Errorf("no symbol at offset %d in %q", offset, uri)
	}
	if target.Symbol.Name == newName {
		return RenamePlan{}, fmt.Errorf("new name %q is unchanged", newName)
	}

	// Collect every occurrence bound to the target identity, per file.
	byFile := map[snapshot.URI][]semantic.Occurrence{}
	for _, u := range w.snapshot().URIs() {
		st := w.files[u]
		if st == nil {
			continue
		}
		for _, occ := range st.analysis.Occurrences {
			if occ.Symbol.ID == target.Symbol.ID {
				byFile[u] = append(byFile[u], occ)
			}
		}
	}

	// Conflict check: the new name must not capture or be captured. Reject if
	// any occurrence already named newName lives in a file the rename would
	// touch (a shadowing local would steal the renamed references, or the
	// rename would hijack an unrelated declaration's uses). For globals also
	// reject any registered global identity with that name, even if its only
	// occurrences sit in untouched files.
	if target.Symbol.LocalKey == "" {
		if _, exists := w.reg.GlobalExisting(newName); exists {
			return RenamePlan{}, fmt.Errorf("name %q already denotes a project-global symbol", newName)
		}
	}
	for u := range byFile {
		st := w.files[u]
		for _, occ := range st.analysis.Occurrences {
			if occ.Symbol.Name == newName && occ.Symbol.ID != target.Symbol.ID {
				return RenamePlan{}, fmt.Errorf(
					"name %q already bound to a different symbol in %s (offset %d); renaming would capture or be shadowed",
					newName, u, occ.Range.Start)
			}
		}
	}

	plan := RenamePlan{
		Version:    w.versionInfoLocked(),
		SymbolID:   target.Symbol.ID,
		SymbolName: target.Symbol.Name,
		NewName:    newName,
	}
	for u, occs := range byFile {
		doc, _ := w.snapshot().Get(u)
		fe := RenameFileEdit{URI: u, Version: doc.Version, Saved: doc.Saved}
		sort.Slice(occs, func(i, j int) bool { return occs[i].Range.Start < occs[j].Range.Start })
		for _, occ := range occs {
			text, err := doc.Slice(occ.Range.Start, occ.Range.End)
			if err != nil {
				return RenamePlan{}, err
			}
			fe.Edits = append(fe.Edits, RenameEdit{
				Range:   Range{Range: occ.Range, Text: text},
				OldText: text,
				NewText: newName,
			})
		}
		plan.Files = append(plan.Files, fe)
	}
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].URI < plan.Files[j].URI })
	return plan, nil
}

// ApplyRename commits a previously prepared plan. It first re-validates the
// plan against the current snapshot: every touched document must still be at
// the previewed version and every edit range must still cover the previewed
// source text. Any mismatch fails with ErrStaleRenamePlan and changes
// nothing. On success all documents are rewritten in one snapshot, so a
// rename can never leave some files renamed and others not.
func (w *Workspace) ApplyRename(plan RenamePlan) (RenameResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Validate everything before touching any document. Freshness is judged
	// per touched document (version + covered bytes), so unrelated edits
	// elsewhere never invalidate a plan, while any change to a file the
	// rename would write forces a fresh preview.
	type pendingDoc struct {
		uri  snapshot.URI
		text string
	}
	var pending []pendingDoc
	for _, fe := range plan.Files {
		doc, ok := w.snapshot().Get(fe.URI)
		if !ok {
			return RenameResult{}, fmt.Errorf("%w: %s no longer open", ErrStaleRenamePlan, fe.URI)
		}
		if doc.Version != fe.Version {
			return RenameResult{}, fmt.Errorf("%w: %s at version %d, plan expected %d",
				ErrStaleRenamePlan, fe.URI, doc.Version, fe.Version)
		}
		text := doc.Text
		// Apply edits from the end of the file backwards so earlier offsets
		// stay valid, verifying the covered bytes as we go.
		edits := append([]RenameEdit(nil), fe.Edits...)
		sort.Slice(edits, func(i, j int) bool { return edits[i].Range.Start > edits[j].Range.Start })
		lastStart := len(text) + 1
		for _, e := range edits {
			start, end := e.Range.Start, e.Range.End
			if start < 0 || end < start || end > len(text) || end > lastStart {
				return RenameResult{}, fmt.Errorf("%w: edit range [%d,%d) invalid for %s",
					ErrStaleRenamePlan, start, end, fe.URI)
			}
			if text[start:end] != e.OldText {
				return RenameResult{}, fmt.Errorf("%w: %s offset %d now holds %q, plan expected %q",
					ErrStaleRenamePlan, fe.URI, start, text[start:end], e.OldText)
			}
			text = text[:start] + e.NewText + text[end:]
			lastStart = start
		}
		pending = append(pending, pendingDoc{uri: fe.URI, text: text})
	}

	// Commit atomically: one new snapshot carrying every rewritten document,
	// then re-analysis. Renamed documents become unsaved overlays; their
	// on-disk bytes are untouched until an explicit Save.
	snap := w.snapshot()
	for _, pd := range pending {
		doc, _ := snap.Get(pd.uri)
		doc.Text = pd.text
		doc.Version++
		doc.Saved = false
		snap = snap.WithDocument(w.nextSnap, doc)
	}
	w.snap = snap
	w.nextSnap++
	for _, pd := range pending {
		w.rebuild(pd.uri)
	}

	res := RenameResult{
		Version:  w.versionInfoLocked(),
		SymbolID: plan.SymbolID,
		NewName:  plan.NewName,
	}
	for _, u := range w.snapshot().URIs() {
		doc, _ := w.snapshot().Get(u)
		res.Documents = append(res.Documents, DocumentInfo{
			URI: u, Version: doc.Version, Saved: doc.Saved, Bytes: len(doc.Text),
		})
	}
	return res, nil
}
