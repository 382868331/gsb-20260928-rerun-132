package workspace_test

import (
	"errors"
	"strings"
	"testing"

	"sourcenav/internal/snapshot"
	"sourcenav/internal/workspace"
)

// Global "top" is shadowed by a local "top" inside main; only the
// occurrences bound to the global symbol may be renamed.
const (
	shadowA = `let top = 10
`
	shadowB = `fn main() {
  let top = 1
  return top + 2
}

fn use() {
  return top
}
`
)

func openShadow(t *testing.T) (*workspace.Workspace, snapshot.URI, snapshot.URI) {
	t.Helper()
	ws := workspace.New()
	uA := snapshot.URI("file:///a.edu")
	uB := snapshot.URI("file:///b.edu")
	ws.Open(uA, shadowA, 1, true)
	ws.Open(uB, shadowB, 1, true)
	return ws, uA, uB
}

func docText(t *testing.T, ws *workspace.Workspace, uri snapshot.URI) string {
	t.Helper()
	text, ok := ws.DocumentText(uri)
	if !ok {
		t.Fatalf("document %s not found", uri)
	}
	return text
}

func docInfo(t *testing.T, ws *workspace.Workspace, uri snapshot.URI) workspace.DocumentInfo {
	t.Helper()
	for _, d := range ws.Status().Documents {
		if d.URI == uri {
			return d
		}
	}
	t.Fatalf("document %s not found", uri)
	return workspace.DocumentInfo{}
}
func TestRenameSkipsShadowedAndUnrelated(t *testing.T) {
	ws, uA, uB := openShadow(t)

	plan, err := ws.PrepareRename(uA, strings.Index(shadowA, "top"), "peak")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Exactly two bound occurrences: declaration in a.edu, use in use() in
	// b.edu. The shadowing local "top" in main must not appear.
	if len(plan.Edits) != 2 {
		t.Fatalf("edits = %d, want 2: %+v", len(plan.Edits), plan.Edits)
	}
	for _, e := range plan.Edits {
		if e.Range.Text != "top" {
			t.Fatalf("edit text = %q", e.Range.Text)
		}
		if e.URI == uB && e.Range.Start != strings.LastIndex(shadowB, "return top")+len("return ") {
			t.Fatalf("unexpected edit offset in b.edu: %+v", e)
		}
	}
	if plan.Declaration == nil || plan.Declaration.URI != uA {
		t.Fatalf("declaration missing: %+v", plan.Declaration)
	}

	res, err := ws.ApplyRename(plan.PlanID, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.AppliedEdits != 2 {
		t.Fatalf("applied = %d, want 2", res.AppliedEdits)
	}

	gotA := docText(t, ws, uA)
	gotB := docText(t, ws, uB)
	if gotA != "let peak = 10\n" {
		t.Fatalf("a.edu = %q", gotA)
	}
	wantB := shadowB[:strings.LastIndex(shadowB, "return top")] + "return peak\n}\n"
	if gotB != wantB {
		t.Fatalf("b.edu = %q, want %q", gotB, wantB)
	}
	if !strings.Contains(gotB, "let top = 1") {
		t.Fatal("shadowed local was rewritten")
	}

	// Re-query after rename: definition of the new name resolves across
	// files and no diagnostics remain.
	def, err := ws.Definition(uB, strings.Index(gotB, "return peak")+len("return "))
	if err != nil {
		t.Fatalf("definition after rename: %v", err)
	}
	if def.SymbolName != "peak" || def.Location == nil || def.Location.URI != uA {
		t.Fatalf("definition after rename = %+v", def)
	}
	for _, u := range []snapshot.URI{uA, uB} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s after rename: %v", u, diags)
		}
	}
}

// Mutually recursive files: renaming one side must update both, and
// navigation must keep working on the new snapshot.
func TestRenameCircularReferences(t *testing.T) {
	ws, uA, uB := openPair(t)

	plan, err := ws.PrepareRename(uA, strings.Index(aSrc, "alpha"), "first")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// alpha: declaration in a.edu + use inside beta in b.edu.
	if len(plan.Edits) != 2 {
		t.Fatalf("edits = %d, want 2: %+v", len(plan.Edits), plan.Edits)
	}
	if _, err := ws.ApplyRename(plan.PlanID, true); err != nil {
		t.Fatalf("apply: %v", err)
	}

	gotA := docText(t, ws, uA)
	gotB := docText(t, ws, uB)
	if !strings.Contains(gotA, "fn first(x)") || strings.Contains(gotA, "alpha") {
		t.Fatalf("a.edu after rename = %q", gotA)
	}
	if !strings.Contains(gotB, "return first(n) + shared") {
		t.Fatalf("b.edu after rename = %q", gotB)
	}

	// Definition of the renamed callee still jumps b.edu -> a.edu.
	def, err := ws.Definition(uB, strings.Index(gotB, "first(n)"))
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if def.Location == nil || def.Location.URI != uA || def.Location.Range.Text != "first" {
		t.Fatalf("definition = %+v", def)
	}
	// The reverse reference (beta used from a.edu) is untouched.
	defB, err := ws.Definition(uA, strings.Index(gotA, "beta(x)"))
	if err != nil || defB.Location == nil || defB.Location.URI != uB {
		t.Fatalf("beta definition broke: %v %+v", err, defB)
	}
	for _, u := range []snapshot.URI{uA, uB} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %v", u, diags)
		}
	}
}

// Inserting text before the declaration after the preview must invalidate
// the plan; nothing may be applied, and a fresh preview must succeed.
func TestRenameStalePlanRejectedAtomically(t *testing.T) {
	ws, uA, uB := openPair(t)

	plan, err := ws.PrepareRename(uB, strings.Index(bSrc, "beta"), "second")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// Concurrent edit: prepend a comment line in the declaration file,
	// shifting every recorded offset.
	edited := "// header\n" + bSrc
	if _, err := ws.ApplyEdit(uB, edited, 2); err != nil {
		t.Fatalf("edit: %v", err)
	}

	_, err = ws.ApplyRename(plan.PlanID, false)
	var stale *workspace.StalePlanError
	if !errors.As(err, &stale) {
		t.Fatalf("apply err = %v, want StalePlanError", err)
	}

	// Atomicity: no file changed — a.edu still references the old name and
	// b.edu is exactly the edited text.
	if got := docText(t, ws, uA); got != aSrc {
		t.Fatalf("a.edu partially modified: %q", got)
	}
	if got := docText(t, ws, uB); got != edited {
		t.Fatalf("b.edu modified despite rejection: %q", got)
	}
	// Old plan is still rejected on retry (no accidental success).
	if _, err := ws.ApplyRename(plan.PlanID, false); err == nil {
		t.Fatal("stale plan applied on retry")
	}

	// Re-preview against the new text and apply.
	plan2, err := ws.PrepareRename(uB, strings.Index(edited, "beta"), "second")
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	if _, err := ws.ApplyRename(plan2.PlanID, false); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if got := docText(t, ws, uB); !strings.Contains(got, "fn second(n)") {
		t.Fatalf("b.edu = %q", got)
	}
	if got := docText(t, ws, uA); !strings.Contains(got, "return second(x) + 1") {
		t.Fatalf("a.edu = %q", got)
	}
	for _, u := range []snapshot.URI{uA, uB} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %v", u, diags)
		}
	}
}

// Two plans prepared back to back: applying the first bumps versions of its
// documents, so an overlapping second plan must be rejected, while edits to
// documents a plan does not touch must not invalidate it.
func TestRenameConcurrentPlansAndEdits(t *testing.T) {
	ws, uA, _ := openPair(t)

	// Plan 1 renames global "shared" (declared in a.edu, used in b.edu).
	p1, err := ws.PrepareRename(uA, strings.Index(aSrc, "shared"), "common")
	if err != nil {
		t.Fatalf("prepare shared: %v", err)
	}
	// Plan 2 renames "alpha" (declared in a.edu) — overlaps a.edu with p1.
	p2, err := ws.PrepareRename(uA, strings.Index(aSrc, "alpha"), "first")
	if err != nil {
		t.Fatalf("prepare alpha: %v", err)
	}

	if _, err := ws.ApplyRename(p1.PlanID, false); err != nil {
		t.Fatalf("apply p1: %v", err)
	}
	// p2's plan for a.edu is now stale.
	var stale *workspace.StalePlanError
	if _, err := ws.ApplyRename(p2.PlanID, false); !errors.As(err, &stale) {
		t.Fatalf("p2 err = %v, want StalePlanError", err)
	}
	// p1 fully applied; nothing of p2 leaked through.
	gotA := docText(t, ws, uA)
	if !strings.Contains(gotA, "let common = 42") || !strings.Contains(gotA, "fn alpha(x)") {
		t.Fatalf("a.edu = %q", gotA)
	}

	// An edit to a document the plan does not touch keeps the plan valid.
	// The shadowing local "top" lives entirely inside b.edu's main.
	ws2, uA2, uB2 := openShadow(t)
	p3, err := ws2.PrepareRename(uB2, strings.Index(shadowB, "let top")+len("let "), "inner")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := ws2.ApplyEdit(uA2, shadowA+"// tail\n", 2); err != nil {
		t.Fatalf("unrelated edit: %v", err)
	}
	if _, err := ws2.ApplyRename(p3.PlanID, false); err != nil {
		t.Fatalf("plan invalidated by unrelated edit: %v", err)
	}
	if got := docText(t, ws2, uB2); !strings.Contains(got, "let inner = 1") {
		t.Fatalf("b.edu = %q", got)
	}
}

// Unsaved buffers participate in analysis, and applying a rename keeps the
// session-text vs persisted distinction explicit via the save flag.
func TestRenameUnsavedOverlayAndSaveFlag(t *testing.T) {
	ws, uA, uB := openPair(t)

	// Push an unsaved change to b.edu that adds another use of "shared".
	unsaved := strings.Replace(bSrc, "return alpha(n) + shared", "return alpha(n) + shared + shared", 1)
	if _, err := ws.ApplyEdit(uB, unsaved, 2); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if docInfo(t, ws, uB).Saved {
		t.Fatal("b.edu should be unsaved after edit")
	}

	// The unsaved text drives the reference set: 3 occurrences of shared.
	plan, err := ws.PrepareRename(uA, strings.Index(aSrc, "shared"), "common")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(plan.Edits) != 3 {
		t.Fatalf("edits = %d, want 3 (unsaved text not analysed): %+v", len(plan.Edits), plan.Edits)
	}

	// save=false: renamed text stays an unsaved session overlay.
	if _, err := ws.ApplyRename(plan.PlanID, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if docInfo(t, ws, uB).Saved {
		t.Fatal("save=false must keep the document unsaved")
	}
	if got := docText(t, ws, uB); strings.Count(got, "common") != 2 {
		t.Fatalf("b.edu = %q", got)
	}

	// save=true: the renamed versions are marked persisted.
	plan2, err := ws.PrepareRename(uA, strings.Index(docText(t, ws, uA), "common"), "mutual")
	if err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	res, err := ws.ApplyRename(plan2.PlanID, true)
	if err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	if !res.Saved {
		t.Fatal("result should report saved=true")
	}
	for _, u := range []snapshot.URI{uA, uB} {
		if !docInfo(t, ws, u).Saved {
			t.Fatalf("%s should be saved after save=true apply", u)
		}
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %v", u, diags)
		}
	}
}

func TestRenameRejectsInvalidAndConflictingNames(t *testing.T) {
	ws, uA, _ := openPair(t)
	off := strings.Index(aSrc, "alpha")
	for _, bad := range []string{"", "1x", "let", "has space", "alpha"} {
		if _, err := ws.PrepareRename(uA, off, bad); err == nil {
			t.Fatalf("new name %q accepted", bad)
		}
	}
	// "beta" is a different existing global: merging identities is refused.
	if _, err := ws.PrepareRename(uA, off, "beta"); err == nil {
		t.Fatal("rename onto existing global accepted")
	}
}
