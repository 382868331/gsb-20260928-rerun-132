package workspace_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"sourcenav/internal/snapshot"
	"sourcenav/internal/workspace"
)

// Mutually referencing files: a calls beta/shared, b calls alpha.
func planFor(t *testing.T, ws *workspace.Workspace, uri snapshot.URI, src, old, newName string) workspace.RenamePlan {
	t.Helper()
	plan, err := ws.PrepareRename(uri, strings.Index(src, old), newName)
	if err != nil {
		t.Fatalf("prepare rename %s -> %s: %v", old, newName, err)
	}
	return plan
}

func TestRenameCrossFileCircular(t *testing.T) {
	ws, uA, uB := openPair(t)

	// Preview renaming beta -> gamma across the circular a<->b reference.
	plan := planFor(t, ws, uA, aSrc, "beta(x)", "gamma")
	if plan.SymbolName != "beta" {
		t.Fatalf("plan symbol = %q", plan.SymbolName)
	}
	// Expect exactly 2 edits: declaration in b, use in a.
	var total int
	files := map[snapshot.URI]workspace.RenameFileEdit{}
	for _, f := range plan.Files {
		files[f.URI] = f
		total += len(f.Edits)
		for _, e := range f.Edits {
			if e.OldText != "beta" || e.NewText != "gamma" {
				t.Fatalf("bad edit %+v", e)
			}
			if e.Range.Text != "beta" {
				t.Fatalf("edit range text %q not real source", e.Range.Text)
			}
		}
	}
	if total != 2 || files[uA].Version != 1 || files[uB].Version != 1 {
		t.Fatalf("plan files wrong: %+v", plan.Files)
	}

	res, err := ws.ApplyRename(plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Rename writes session text only: every touched doc is now unsaved.
	for _, d := range res.Documents {
		if (d.URI == uA || d.URI == uB) && d.Saved {
			t.Fatalf("%s should be unsaved after rename", d.URI)
		}
	}

	// Re-query definition: gamma use in a must jump to b's declaration.
	def, err := ws.Definition(uA, strings.Index(aSrc, "beta(x)")) // same offset, text now gamma
	if err != nil {
		t.Fatalf("definition after rename: %v", err)
	}
	if def.SymbolName != "gamma" || def.Location == nil || def.Location.URI != uB {
		t.Fatalf("gamma should resolve into %s: %+v", uB, def)
	}
	if def.Location.Range.Text != "gamma" {
		t.Fatalf("declaration text %q", def.Location.Range.Text)
	}
	// Diagnostics must be clean in both files after the rename.
	for _, u := range []snapshot.URI{uA, uB} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s after rename: %+v", u, diags)
		}
	}

	// Reference set of gamma is exactly the renamed set.
	refs, err := ws.References(uB, def.Location.Range.Start)
	if err != nil {
		t.Fatal(err)
	}
	count := len(refs.References)
	if refs.Declaration != nil {
		count++
	}
	if count != 2 {
		t.Fatalf("gamma occurrences = %d, want 2", count)
	}
}

func TestRenameSkipsShadowingAndUnrelated(t *testing.T) {
	ws := workspace.New()
	u := snapshot.URI("file:///shadow.edu")
	ws.Open(u, shadowSrc, 1, true)

	// Rename the inner let-v (inside the if block) only.
	innerUse := indexNth(shadowSrc, "v", 5)
	plan, err := ws.PrepareRename(u, innerUse, "renamed")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	var total int
	for _, f := range plan.Files {
		total += len(f.Edits)
	}
	if total != 2 { // decl "let v = 9" + "return v" inside the if
		t.Fatalf("inner v edits = %d, want 2 (shadowing excluded)", total)
	}
	if _, err := ws.ApplyRename(plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Global v and parameter v must be untouched: other() still binds to
	// line-1 global, and no occurrence anywhere is named "renamed" except
	// the two inner ones. Offsets are computed against the renamed text.
	newSrc := strings.Replace(shadowSrc, "let v = 9", "let renamed = 9", 1)
	newSrc = strings.Replace(newSrc, "return v", "return renamed", 1)
	otherUse := indexNth(newSrc, "return v", 2) + len("return ")
	def, err := ws.Definition(u, otherUse)
	if err != nil {
		t.Fatalf("global v definition: %v", err)
	}
	if def.SymbolName != "v" || def.Location == nil || def.Location.Range.StartLine != 1 {
		t.Fatalf("global v disturbed by local rename: %+v", def)
	}
	refs, err := ws.References(u, otherUse)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range refs.References {
		if l.Range.Text != "v" {
			t.Fatalf("unexpected renamed occurrence in global v refs: %q", l.Range.Text)
		}
	}
	if diags := ws.Diagnostics(u); len(diags) != 0 {
		t.Fatalf("diagnostics after shadow rename: %+v", diags)
	}
}

func TestRenameConflictWithExistingName(t *testing.T) {
	ws, uA, _ := openPair(t)
	// alpha -> shared would collide with the existing global shared.
	if _, err := ws.PrepareRename(uA, strings.Index(aSrc, "alpha"), "shared"); err == nil {
		t.Fatal("expected conflict renaming alpha to existing global shared")
	}
	// Renaming to a keyword / bad identifier is rejected.
	if _, err := ws.PrepareRename(uA, strings.Index(aSrc, "alpha"), "let"); err == nil {
		t.Fatal("expected rejection of keyword as new name")
	}
	if _, err := ws.PrepareRename(uA, strings.Index(aSrc, "alpha"), "9x"); err == nil {
		t.Fatal("expected rejection of invalid identifier")
	}
}

func TestRenameStalePlanRejectedNoPartialState(t *testing.T) {
	ws, uA, uB := openPair(t)
	plan := planFor(t, ws, uA, aSrc, "beta(x)", "gamma")

	// Text inserted before beta's declaration in b shifts every offset.
	prefixed := "// header comment\n" + bSrc
	if _, err := ws.ApplyEdit(uB, prefixed, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ApplyRename(plan); !errors.Is(err, workspace.ErrStaleRenamePlan) {
		t.Fatalf("expected stale plan error, got %v", err)
	}

	// Nothing was renamed: a unchanged, b only has the prefix edit.
	def, err := ws.Definition(uA, strings.Index(aSrc, "beta(x)"))
	if err != nil || def.SymbolName != "beta" {
		t.Fatalf("partial rename happened: %+v %v", def, err)
	}
	if def.Location == nil || def.Location.URI != uB || def.Location.Version != 2 {
		t.Fatalf("beta declaration should be at b v2: %+v", def.Location)
	}
	for _, u := range []snapshot.URI{uA, uB} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s after rejected rename: %+v", u, diags)
		}
	}

	// Re-preparing against the new snapshot works and applies cleanly.
	plan2, err := ws.PrepareRename(uA, strings.Index(aSrc, "beta(x)"), "gamma")
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	if _, err := ws.ApplyRename(plan2); err != nil {
		t.Fatalf("apply re-prepared plan: %v", err)
	}
	def2, err := ws.Definition(uA, strings.Index(aSrc, "beta(x)"))
	if err != nil || def2.SymbolName != "gamma" {
		t.Fatalf("gamma not resolved after re-prepared rename: %+v %v", def2, err)
	}
}

func TestRenameConcurrentEditsInvalidate(t *testing.T) {
	ws, uA, uB := openPair(t)
	plan := planFor(t, ws, uA, aSrc, "beta(x)", "gamma")

	// Two concurrent unsaved edits land after the preview, one per file.
	var wg sync.WaitGroup
	for i, job := range []struct {
		uri  snapshot.URI
		text string
	}{
		{uA, strings.Replace(aSrc, "42", "43", 1)},
		{uB, strings.Replace(bSrc, "shared", "shared", 1) + "// concurrent\n"},
	} {
		wg.Add(1)
		go func(i int, uri snapshot.URI, text string) {
			defer wg.Done()
			if _, err := ws.ApplyEdit(uri, text, 2+i); err != nil {
				t.Errorf("concurrent edit: %v", err)
			}
		}(i, job.uri, job.text)
	}
	wg.Wait()

	if _, err := ws.ApplyRename(plan); !errors.Is(err, workspace.ErrStaleRenamePlan) {
		t.Fatalf("expected stale plan after concurrent edits, got %v", err)
	}
	// Reference set of beta is still coherent: decl in b + use in a.
	refs, err := ws.References(uA, strings.Index(aSrc, "beta(x)"))
	if err != nil {
		t.Fatal(err)
	}
	count := len(refs.References)
	if refs.Declaration != nil {
		count++
	}
	if count != 2 {
		t.Fatalf("beta occurrences after rejected rename = %d, want 2", count)
	}
}

func TestRenameUnrelatedEditKeepsPlanFresh(t *testing.T) {
	ws, uA, _ := openPair(t)
	uC := snapshot.URI("file:///c.edu")
	ws.Open(uC, "let other = 1\n", 1, true)

	plan := planFor(t, ws, uA, aSrc, "beta(x)", "gamma")
	// An edit to a file the plan does not touch must not invalidate it.
	if _, err := ws.ApplyEdit(uC, "let other = 2\n", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ApplyRename(plan); err != nil {
		t.Fatalf("unrelated edit should not stale the plan: %v", err)
	}
	def, err := ws.Definition(uA, strings.Index(aSrc, "beta(x)"))
	if err != nil || def.SymbolName != "gamma" {
		t.Fatalf("gamma unresolved: %+v %v", def, err)
	}
}

func TestRenameIncludesUnsavedOverlay(t *testing.T) {
	ws := workspace.New()
	uA := snapshot.URI("file:///a.edu")
	uB := snapshot.URI("file:///b.edu")
	// b is an unsaved buffer (saved:false); analysis must still see it.
	ws.Open(uA, "fn main() { return helper(1) }\n", 1, true)
	ws.Open(uB, "fn helper(n) { return n }\n", 1, false)

	plan := planFor(t, ws, uA, "fn main() { return helper(1) }\n", "helper(1)", "util")
	var bEdits int
	for _, f := range plan.Files {
		if f.URI == uB {
			bEdits = len(f.Edits)
			if f.Saved {
				t.Fatal("plan must record b as unsaved")
			}
		}
	}
	if bEdits != 1 {
		t.Fatalf("unsaved buffer declaration missing from plan")
	}
	res, err := ws.ApplyRename(plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, d := range res.Documents {
		if d.Saved {
			t.Fatalf("%s must be unsaved after rename (session text only)", d.URI)
		}
	}
	// Definition from a now resolves to the renamed unsaved declaration.
	def, err := ws.Definition(uA, strings.Index("fn main() { return helper(1) }\n", "helper(1)"))
	if err != nil || def.SymbolName != "util" || def.Location.URI != uB {
		t.Fatalf("util unresolved after rename: %+v %v", def, err)
	}
	// Save flips only the persistence marker; identity survives.
	if _, err := ws.Save(uB); err != nil {
		t.Fatal(err)
	}
	def2, err := ws.Definition(uA, strings.Index("fn main() { return helper(1) }\n", "helper(1)"))
	if err != nil || def2.SymbolID != def.SymbolID {
		t.Fatal("save must not change symbol identity")
	}
}
