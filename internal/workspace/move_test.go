package workspace_test

import (
	"errors"
	"strings"
	"testing"

	"sourcenav/internal/snapshot"
	"sourcenav/internal/workspace"
)

// Three files: app.a declares helper, app.b calls it qualified, and a second
// app.a file calls it bare inside the same module.
const (
	modASrc = `module "app.a"

fn helper(x) {
  return x + 1
}
`
	modBSrc = `module "app.b"

import "app.a"

fn run(y) {
  return a.helper(y)
}
`
	modCSrc = `module "app.a"

fn use(z) {
  return helper(z)
}
`
)

func openModuleTrio(t *testing.T) (*workspace.Workspace, snapshot.URI, snapshot.URI, snapshot.URI) {
	t.Helper()
	ws := workspace.New()
	uA := snapshot.URI("file:///a.edu")
	uB := snapshot.URI("file:///b.edu")
	uC := snapshot.URI("file:///c.edu")
	ws.Open(uA, modASrc, 1, true)
	ws.Open(uB, modBSrc, 1, true)
	ws.Open(uC, modCSrc, 1, true)
	return ws, uA, uB, uC
}

// Moving a declaration across files with a rename keeps every deliberately
// untouched reference resolving through the compat entry, under the same
// symbol identity.
func TestMoveAcrossFilesKeepsReferencesResolving(t *testing.T) {
	ws, uA, uB, uC := openModuleTrio(t)

	selOff := strings.Index(modBSrc, "helper") // the selector in a.helper(y)
	before, err := ws.Definition(uB, selOff)
	if err != nil {
		t.Fatalf("definition before move: %v", err)
	}
	if before.Location == nil || before.Location.URI != uA {
		t.Fatalf("helper should be declared in %s, got %+v", uA, before.Location)
	}

	declOff := strings.Index(modASrc, "helper")
	plan, err := ws.PrepareMove(uA, declOff, uB, "util", true)
	if err != nil {
		t.Fatalf("prepare move: %v", err)
	}
	if len(plan.Files) != 2 {
		t.Fatalf("plan should touch 2 files, got %d", len(plan.Files))
	}
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("apply move: %v", err)
	}

	// The declaration landed in b.edu under the new name; the source module
	// keeps a compat entry under the old name.
	st := ws.Status()
	var aInfo, bInfo workspace.DocumentInfo
	for _, d := range st.Documents {
		switch d.URI {
		case uA:
			aInfo = d
		case uB:
			bInfo = d
		}
	}
	if aInfo.Saved || bInfo.Saved {
		t.Fatal("touched documents must become unsaved overlays")
	}
	if aInfo.Version != 2 || bInfo.Version != 2 {
		t.Fatalf("touched documents should be at version 2, got a=%d b=%d", aInfo.Version, bInfo.Version)
	}

	// Kept qualified reference in b.edu: same identity, resolves to the
	// compat entry in a.edu.
	after, err := ws.Definition(uB, selOff)
	if err != nil {
		t.Fatalf("definition after move: %v", err)
	}
	if after.SymbolID != before.SymbolID {
		t.Fatalf("symbol identity changed across move: %d -> %d", before.SymbolID, after.SymbolID)
	}
	if after.Location == nil || after.Location.URI != uA {
		t.Fatalf("kept reference should resolve to the compat entry in %s, got %+v", uA, after.Location)
	}
	if after.Location.Range.Text != "helper" {
		t.Fatalf("compat entry text = %q, want helper", after.Location.Range.Text)
	}

	// Kept bare reference inside the same module (c.edu) also resolves
	// through the compat entry with the same identity.
	bareOff := strings.Index(modCSrc, "helper(z)")
	bare, err := ws.Definition(uC, bareOff)
	if err != nil {
		t.Fatalf("bare definition after move: %v", err)
	}
	if bare.SymbolID != before.SymbolID {
		t.Fatalf("bare reference identity changed: %d -> %d", before.SymbolID, bare.SymbolID)
	}
	if bare.Location == nil || bare.Location.URI != uA {
		t.Fatalf("bare reference should resolve to the compat entry in %s, got %+v", uA, bare.Location)
	}

	// No file may report undeclared names after the migration.
	for _, u := range []snapshot.URI{uA, uB, uC} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("unexpected diagnostics in %s: %+v", u, diags)
		}
	}

	// References see the whole family: the migrated declaration in b.edu
	// plus the kept uses in b.edu and c.edu.
	refs, err := ws.References(uB, selOff)
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if refs.Declaration == nil || refs.Declaration.URI != uB || refs.Declaration.Range.Text != "util" {
		t.Fatalf("declaration should be the migrated fn util in %s, got %+v", uB, refs.Declaration)
	}
	seen := map[snapshot.URI]bool{}
	for _, l := range refs.References {
		seen[l.URI] = true
	}
	if !seen[uB] || !seen[uC] {
		t.Fatalf("kept references missing: %+v", refs.References)
	}
}

// The compat entry is a re-export in the source module: the source file
// imports the destination module and re-exports the moved symbol under its
// old name.
func TestMoveCompatEntryContents(t *testing.T) {
	ws, uA, uB, _ := openModuleTrio(t)

	declOff := strings.Index(modASrc, "helper")
	plan, err := ws.PrepareMove(uA, declOff, uB, "util", true)
	if err != nil {
		t.Fatalf("prepare move: %v", err)
	}
	var src, dst string
	for _, f := range plan.Files {
		switch f.URI {
		case uA:
			src = f.NewText
		case uB:
			dst = f.NewText
		}
	}
	if !strings.Contains(src, `import "app.b"`) {
		t.Fatalf("source should import the destination module, got:\n%s", src)
	}
	if !strings.Contains(src, "export util as helper") {
		t.Fatalf("source should re-export under the old name, got:\n%s", src)
	}
	if strings.Contains(src, "fn helper") {
		t.Fatalf("declaration should be removed from source, got:\n%s", src)
	}
	if !strings.Contains(dst, "fn util(x)") {
		t.Fatalf("destination should declare util, got:\n%s", dst)
	}
	if strings.Contains(dst, "fn helper") {
		t.Fatalf("destination must not keep the old name, got:\n%s", dst)
	}
}

// Undo restores the original declaration, references and file states; a
// definition query afterwards lands on the original declaration again.
func TestMoveUndoRestoresDefinition(t *testing.T) {
	ws, uA, uB, uC := openModuleTrio(t)

	selOff := strings.Index(modBSrc, "helper")
	before, err := ws.Definition(uB, selOff)
	if err != nil {
		t.Fatalf("definition before move: %v", err)
	}

	declOff := strings.Index(modASrc, "helper")
	plan, err := ws.PrepareMove(uA, declOff, uB, "util", true)
	if err != nil {
		t.Fatalf("prepare move: %v", err)
	}
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("apply move: %v", err)
	}

	st, err := ws.UndoMove()
	if err != nil {
		t.Fatalf("undo move: %v", err)
	}
	// File states (version + saved flag + size) are exactly as before.
	versions := map[snapshot.URI]workspace.DocumentInfo{}
	for _, d := range st.Documents {
		versions[d.URI] = d
	}
	for _, u := range []snapshot.URI{uA, uB, uC} {
		info := versions[u]
		if info.Version != 1 || !info.Saved {
			t.Fatalf("%s not restored: version=%d saved=%v", u, info.Version, info.Saved)
		}
	}
	if versions[uA].Bytes != len(modASrc) || versions[uB].Bytes != len(modBSrc) {
		t.Fatal("document texts not restored to original sizes")
	}

	// Definition query after undo: back to the original declaration in
	// a.edu, with the original identity.
	after, err := ws.Definition(uB, selOff)
	if err != nil {
		t.Fatalf("definition after undo: %v", err)
	}
	if after.SymbolID != before.SymbolID {
		t.Fatalf("identity not restored: %d -> %d", before.SymbolID, after.SymbolID)
	}
	if after.Location == nil || after.Location.URI != uA {
		t.Fatalf("after undo helper should be declared in %s, got %+v", uA, after.Location)
	}
	if after.Location.Range.Text != "helper" || after.Location.Range.StartLine != 3 {
		t.Fatalf("original declaration not restored: %+v", after.Location)
	}
	if after.Location.Version != 1 {
		t.Fatalf("declaration should be bound to restored version 1, got %d", after.Location.Version)
	}

	// The bare reference in c.edu resolves to the original declaration too.
	bareOff := strings.Index(modCSrc, "helper(z)")
	bare, err := ws.Definition(uC, bareOff)
	if err != nil {
		t.Fatalf("bare definition after undo: %v", err)
	}
	if bare.Location == nil || bare.Location.URI != uA || bare.Location.Range.StartLine != 3 {
		t.Fatalf("bare reference should bind to the restored declaration, got %+v", bare.Location)
	}
	for _, u := range []snapshot.URI{uA, uB, uC} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("unexpected diagnostics after undo in %s: %+v", u, diags)
		}
	}

	// The undo stack is consumed.
	if _, err := ws.UndoMove(); err == nil {
		t.Fatal("second undo should fail")
	}
}

// A plan computed against an older document version is rejected wholesale.
func TestMoveStalePlanRejected(t *testing.T) {
	ws, uA, uB, _ := openModuleTrio(t)

	declOff := strings.Index(modASrc, "helper")
	plan, err := ws.PrepareMove(uA, declOff, uB, "util", true)
	if err != nil {
		t.Fatalf("prepare move: %v", err)
	}
	if _, err := ws.ApplyEdit(uB, modBSrc+"// touched\n", 2); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := ws.ApplyMove(plan); !errors.Is(err, workspace.ErrStaleMovePlan) {
		t.Fatalf("expected stale plan error, got %v", err)
	}
	// Nothing was applied: the declaration is still in a.edu.
	selOff := strings.Index(modBSrc, "helper")
	def, err := ws.Definition(uB, selOff)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if def.Location == nil || def.Location.URI != uA {
		t.Fatalf("stale move must not change anything, got %+v", def.Location)
	}
}
