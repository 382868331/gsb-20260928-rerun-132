// Rename support: two-phase cross-file renaming with preview.
//
// PrepareRename computes the exact set of occurrences bound to the target
// symbol (shadowed or unrelated same-named identifiers are never included)
// and freezes it into a plan tied to concrete document versions. ApplyRename
// re-validates every affected document before touching anything: if any
// related document changed since the preview, the plan is rejected instead of
// writing stale offsets into new source. Application is atomic — either every
// file moves to its renamed version together or nothing changes.
package workspace

import (
	"fmt"
	"sort"

	"sourcenav/internal/semantic"
	"sourcenav/internal/snapshot"
	"sourcenav/internal/syntax"
)

// RenameEdit is one planned identifier replacement, bound to a document
// version and carrying the exact source text it expects to replace.
type RenameEdit struct {
	URI     snapshot.URI `json:"uri"`
	Version int          `json:"version"`
	Range   Range        `json:"range"`
	Role    string       `json:"role"`
	NewText string       `json:"newText"`
}

// RenamePlan is the preview of a rename: every edit that ApplyRename will
// perform, plus the snapshot/document versions the plan is valid against.
type RenamePlan struct {
	PlanID      int64        `json:"planId"`
	Version     VersionInfo  `json:"version"`
	SymbolID    uint64       `json:"symbolId"`
	OldName     string       `json:"oldName"`
	NewName     string       `json:"newName"`
	Declaration *Location    `json:"declaration,omitempty"`
	Edits       []RenameEdit `json:"edits"`
}

// RenameResult reports a successfully applied rename.
type RenameResult struct {
	PlanID       int64       `json:"planId"`
	OldName      string      `json:"oldName"`
	NewName      string      `json:"newName"`
	AppliedEdits int         `json:"appliedEdits"`
	Saved        bool        `json:"saved"`
	Version      VersionInfo `json:"version"`
}

// StalePlanError is returned when a related document changed after the
// preview, so the plan's offsets can no longer be trusted.
type StalePlanError struct {
	PlanID  int64
	URI     snapshot.URI
	Want    int
	Got     int
	Missing bool
}

func (e *StalePlanError) Error() string {
	if e.Missing {
		return fmt.Sprintf("rename plan %d is stale: document %q no longer exists; re-run the rename preview", e.PlanID, e.URI)
	}
	return fmt.Sprintf("rename plan %d is stale: document %q changed from version %d to %d; re-run the rename preview",
		e.PlanID, e.URI, e.Want, e.Got)
}

type plannedEdit struct {
	uri        snapshot.URI
	start, end int
	oldText    string
}

type renamePlanState struct {
	symbolID uint64
	oldName  string
	newName  string
	versions map[snapshot.URI]int
	edits    []plannedEdit
}

// PrepareRename resolves the identifier at uri/offset and builds a rename
// plan covering exactly the occurrences bound to that symbol — the
// declaration plus every reference that resolves to it. Same-named
// identifiers in shadowing or unrelated scopes have different symbol
// identities and are never included.
func (w *Workspace) PrepareRename(uri snapshot.URI, offset int, newName string) (RenamePlan, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !syntax.IsIdentifier(newName) {
		return RenamePlan{}, fmt.Errorf("invalid new name %q: not an Edulang identifier", newName)
	}
	o, ok := w.findOccurrence(uri, offset)
	if !ok {
		if perr := w.parseErrs[uri]; perr != nil {
			return RenamePlan{}, perr
		}
		return RenamePlan{}, fmt.Errorf("no symbol at offset %d in %q", offset, uri)
	}
	if o.Symbol.Name == newName {
		return RenamePlan{}, fmt.Errorf("new name %q is identical to the current name", newName)
	}
	// Renaming a global onto an existing different global would merge two
	// identities; refuse instead of silently corrupting resolution.
	if o.Symbol.LocalKey == "" {
		if existing, ok := w.reg.GlobalExisting(newName); ok && existing.ID != o.Symbol.ID {
			return RenamePlan{}, fmt.Errorf("cannot rename %q to %q: a different global symbol with that name already exists", o.Symbol.Name, newName)
		}
	}

	state := &renamePlanState{
		symbolID: o.Symbol.ID,
		oldName:  o.Symbol.Name,
		newName:  newName,
		versions: make(map[snapshot.URI]int),
	}
	plan := RenamePlan{
		Version:  w.versionInfoLocked(),
		SymbolID: o.Symbol.ID,
		OldName:  o.Symbol.Name,
		NewName:  newName,
	}
	for _, u := range w.snapshot().URIs() {
		st := w.files[u]
		if st == nil {
			continue
		}
		doc, _ := w.snapshot().Get(u)
		for _, occ := range st.analysis.Occurrences {
			if occ.Symbol.ID != o.Symbol.ID {
				continue
			}
			text, err := doc.Slice(occ.Range.Start, occ.Range.End)
			if err != nil || text != o.Symbol.Name {
				return RenamePlan{}, fmt.Errorf("internal: occurrence range [%d,%d) in %q does not slice to %q",
					occ.Range.Start, occ.Range.End, u, o.Symbol.Name)
			}
			role := "reference"
			if occ.Role == semantic.RoleDecl {
				role = "declaration"
				loc := w.toLocation(occ)
				if plan.Declaration == nil {
					plan.Declaration = &loc
				}
			}
			plan.Edits = append(plan.Edits, RenameEdit{
				URI: u, Version: doc.Version,
				Range:   Range{Range: occ.Range, Text: text},
				Role:    role,
				NewText: newName,
			})
			state.edits = append(state.edits, plannedEdit{
				uri: u, start: occ.Range.Start, end: occ.Range.End, oldText: text,
			})
			state.versions[u] = doc.Version
		}
	}
	if len(plan.Edits) == 0 {
		return RenamePlan{}, fmt.Errorf("symbol %q has no occurrences to rename", o.Symbol.Name)
	}
	sort.Slice(plan.Edits, func(i, j int) bool {
		if plan.Edits[i].URI != plan.Edits[j].URI {
			return plan.Edits[i].URI < plan.Edits[j].URI
		}
		return plan.Edits[i].Range.Start < plan.Edits[j].Range.Start
	})
	plan.PlanID = w.nextPlan
	w.nextPlan++
	w.plans[plan.PlanID] = state
	return plan, nil
}

// ApplyRename commits a previously prepared plan. Every affected document is
// re-validated against the versions captured at preview time and each planned
// edit's source text is re-checked at its recorded offset; any mismatch
// rejects the whole plan with a StalePlanError before a single byte changes.
// When validation passes, all documents are replaced together (no partial
// application). save=false keeps the renamed text as an unsaved session
// overlay; save=true additionally marks the new versions as persisted.
func (w *Workspace) ApplyRename(planID int64, save bool) (RenameResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	plan, ok := w.plans[planID]
	if !ok {
		return RenameResult{}, fmt.Errorf("unknown or already applied rename plan %d", planID)
	}

	// Version gate: every document that contributed edits must be untouched.
	for u, want := range plan.versions {
		doc, ok := w.snapshot().Get(u)
		if !ok {
			return RenameResult{}, &StalePlanError{PlanID: planID, URI: u, Want: want, Missing: true}
		}
		if doc.Version != want {
			return RenameResult{}, &StalePlanError{PlanID: planID, URI: u, Want: want, Got: doc.Version}
		}
	}

	// Content gate: re-slice the current text at every planned offset.
	byURI := make(map[snapshot.URI][]plannedEdit)
	for _, e := range plan.edits {
		byURI[e.uri] = append(byURI[e.uri], e)
	}
	newTexts := make(map[snapshot.URI]string, len(byURI))
	for u, edits := range byURI {
		doc, _ := w.snapshot().Get(u)
		text := doc.Text
		sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
		for _, e := range edits {
			if e.start < 0 || e.end > len(text) || text[e.start:e.end] != e.oldText {
				return RenameResult{}, &StalePlanError{PlanID: planID, URI: u, Want: plan.versions[u], Got: doc.Version}
			}
			text = text[:e.start] + plan.newName + text[e.end:]
		}
		newTexts[u] = text
	}

	// Atomic commit: build the full successor snapshot first, then swap.
	snap := w.snapshot()
	for u, text := range newTexts {
		doc, _ := snap.Get(u)
		doc.Text = text
		doc.Version++
		doc.Saved = save
		snap = snap.WithDocument(w.nextSnap, doc)
		w.nextSnap++
	}
	w.snap = snap
	for u := range newTexts {
		w.rebuild(u)
	}
	delete(w.plans, planID)

	return RenameResult{
		PlanID:       planID,
		OldName:      plan.oldName,
		NewName:      plan.newName,
		AppliedEdits: len(plan.edits),
		Saved:        save,
		Version:      w.versionInfoLocked(),
	}, nil
}
