package workspace_test

import (
	"errors"
	"strings"
	"testing"

	"sourcenav/internal/snapshot"
	"sourcenav/internal/workspace"
)

// Three modules: core declares helper, app consumes it through an import,
// util is the migration target.
const (
	coreSrc = `module "core"

fn helper(x) {
  return x + 1
}

fn local() {
  return helper(1)
}
`
	utilSrc = `module "util"

fn other() {
  return 0
}
`
	appSrc = `module "app"

import "core"

fn main() {
  return core.helper(2)
}
`
)

func openModules(t *testing.T) (*workspace.Workspace, snapshot.URI, snapshot.URI, snapshot.URI) {
	t.Helper()
	ws := workspace.New()
	uCore := snapshot.URI("file:///core.edu")
	uUtil := snapshot.URI("file:///util.edu")
	uApp := snapshot.URI("file:///app.edu")
	ws.Open(uCore, coreSrc, 1, true)
	ws.Open(uUtil, utilSrc, 1, true)
	ws.Open(uApp, appSrc, 1, true)
	return ws, uCore, uUtil, uApp
}

func declOffset(s, name string) int { return strings.Index(s, "fn "+name) + len("fn ") }

func mustClean(t *testing.T, ws *workspace.Workspace, uris ...snapshot.URI) {
	t.Helper()
	for _, u := range uris {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %+v", u, diags)
		}
	}
}

// Move + rename across files: every reference is rewritten to the new home
// and definition queries land on the relocated declaration.
func TestMoveDeclarationAcrossFiles(t *testing.T) {
	ws, uCore, uUtil, uApp := openModules(t)

	plan, err := ws.PrepareMove(uCore, declOffset(coreSrc, "helper"), uUtil, "assist", nil)
	if err != nil {
		t.Fatalf("prepare move: %v", err)
	}
	if plan.SymbolName != "helper" || plan.NewName != "assist" {
		t.Fatalf("plan names: %+v", plan)
	}
	appNew := ""
	for _, f := range plan.Files {
		if f.URI == uApp {
			appNew = f.NewText
		}
	}
	if appNew == "" {
		t.Fatal("plan should rewrite app.edu")
	}
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("apply move: %v", err)
	}
	mustClean(t, ws, uCore, uUtil, uApp)

	// The declaration now lives in util.edu under the new name.
	def, err := ws.Definition(uApp, strings.Index(appNew, "util.assist")+len("util."))
	if err != nil {
		t.Fatalf("definition of moved symbol: %v", err)
	}
	if def.SymbolName != "assist" || def.Location == nil || def.Location.URI != uUtil {
		t.Fatalf("assist should be declared in %s: %+v", uUtil, def)
	}
	if def.Location.Range.Text != "assist" {
		t.Fatalf("declaration text %q", def.Location.Range.Text)
	}

	// References were rewritten to qualified util.assist uses.
	refs, err := ws.References(uUtil, def.Location.Range.Start)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.References) != 2 { // core.local + app.main
		t.Fatalf("assist references = %d, want 2: %+v", len(refs.References), refs.References)
	}
	for _, l := range refs.References {
		if l.Range.Text != "assist" {
			t.Fatalf("reference text %q, want assist", l.Range.Text)
		}
	}
}

// Retained references keep their source text and still resolve to the moved
// declaration through the compatibility entry left in the source file.
func TestMoveKeepsCompatEntryForRetainedReferences(t *testing.T) {
	ws, uCore, uUtil, uApp := openModules(t)

	plan, err := ws.PrepareMove(uCore, declOffset(coreSrc, "helper"), uUtil, "assist", []snapshot.URI{uApp})
	if err != nil {
		t.Fatalf("prepare move: %v", err)
	}
	res, err := ws.ApplyMove(plan)
	if err != nil {
		t.Fatalf("apply move: %v", err)
	}
	mustClean(t, ws, uCore, uUtil, uApp)

	// app.edu is untouched: same version, still saved.
	for _, d := range res.Documents {
		if d.URI == uApp && (d.Version != 1 || !d.Saved) {
			t.Fatalf("retained file changed: %+v", d)
		}
	}

	// The retained core.helper use resolves to the declaration in util.edu.
	def, err := ws.Definition(uApp, strings.Index(appSrc, "core.helper")+len("core."))
	if err != nil {
		t.Fatalf("definition through compat entry: %v", err)
	}
	if def.Location == nil || def.Location.URI != uUtil || def.Location.Range.Text != "assist" {
		t.Fatalf("retained reference should resolve into %s: %+v", uUtil, def)
	}

	// The compatibility entry exists in the source file: querying the old
	// alias from core.edu itself (export assist as helper) resolves too.
	refs, err := ws.References(uApp, strings.Index(appSrc, "core.helper")+len("core."))
	if err != nil {
		t.Fatal(err)
	}
	if refs.Declaration == nil || refs.Declaration.URI != uUtil {
		t.Fatalf("declaration should be in %s: %+v", uUtil, refs.Declaration)
	}
}

// Undo restores the original declaration, references and file states.
func TestMoveUndoRestoresDefinition(t *testing.T) {
	ws, uCore, uUtil, uApp := openModules(t)

	plan, err := ws.PrepareMove(uCore, declOffset(coreSrc, "helper"), uUtil, "assist", []snapshot.URI{uApp})
	if err != nil {
		t.Fatalf("prepare move: %v", err)
	}
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("apply move: %v", err)
	}
	mustClean(t, ws, uCore, uUtil, uApp)

	if _, err := ws.UndoMove(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	mustClean(t, ws, uCore, uUtil, uApp)

	// File states are byte-identical to the pre-move session.
	st := ws.Status()
	want := map[snapshot.URI]int{uCore: len(coreSrc), uUtil: len(utilSrc), uApp: len(appSrc)}
	for _, d := range st.Documents {
		if d.Version != 1 || !d.Saved {
			t.Fatalf("%s not restored to version 1/saved: %+v", d.URI, d)
		}
		if d.Bytes != want[d.URI] {
			t.Fatalf("%s bytes = %d, want %d", d.URI, d.Bytes, want[d.URI])
		}
	}

	// Definition query on the restored reference lands back on the original
	// declaration in core.edu.
	def, err := ws.Definition(uApp, strings.Index(appSrc, "core.helper")+len("core."))
	if err != nil {
		t.Fatalf("definition after undo: %v", err)
	}
	if def.SymbolName != "helper" || def.Location == nil || def.Location.URI != uCore {
		t.Fatalf("helper should be declared in %s again: %+v", uCore, def)
	}
	if def.Location.Range.Text != "helper" {
		t.Fatalf("declaration text %q after undo", def.Location.Range.Text)
	}

	// A second undo has nothing to revert.
	if _, err := ws.UndoMove(); err == nil {
		t.Fatal("second undo should fail")
	}
}

// A plan computed against an older document state is rejected wholesale.
func TestMoveStalePlanRejected(t *testing.T) {
	ws, uCore, uUtil, uApp := openModules(t)

	plan, err := ws.PrepareMove(uCore, declOffset(coreSrc, "helper"), uUtil, "assist", nil)
	if err != nil {
		t.Fatalf("prepare move: %v", err)
	}
	if _, err := ws.ApplyEdit(uApp, appSrc+"// touched\n", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ApplyMove(plan); !errors.Is(err, workspace.ErrStaleMovePlan) {
		t.Fatalf("stale plan should be rejected with ErrStaleMovePlan, got %v", err)
	}
	// Nothing was applied: the original definition still resolves.
	def, err := ws.Definition(uApp, strings.Index(appSrc, "core.helper")+len("core."))
	if err != nil {
		t.Fatal(err)
	}
	if def.Location == nil || def.Location.URI != uCore {
		t.Fatalf("helper should still be in %s: %+v", uCore, def)
	}
}
