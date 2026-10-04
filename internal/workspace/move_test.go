package workspace_test

import (
	"errors"
	"strings"
	"testing"

	"sourcenav/internal/snapshot"
	"sourcenav/internal/workspace"
)

const moveASrc = `let top = 10

fn add(a, b) {
  return a + b + top
}
`

const moveBSrc = `fn main() {
  return add(top, 2)
}
`

const moveCSrc = `fn helper() {
  return 1
}
`

func openABC(t *testing.T) (*workspace.Workspace, snapshot.URI, snapshot.URI, snapshot.URI) {
	t.Helper()
	ws := workspace.New()
	uA, uB, uC := snapshot.URI("file:///a.edu"), snapshot.URI("file:///b.edu"), snapshot.URI("file:///c.edu")
	ws.Open(uA, moveASrc, 1, true)
	ws.Open(uB, moveBSrc, 1, true)
	ws.Open(uC, moveCSrc, 1, true)
	return ws, uA, uB, uC
}

func moveSpec(uri snapshot.URI, src, name string, extra ...func(*workspace.MoveItemSpec)) workspace.MoveItemSpec {
	spec := workspace.MoveItemSpec{URI: uri, Offset: strings.Index(src, name)}
	for _, f := range extra {
		f(&spec)
	}
	return spec
}

func withNewName(n string) func(*workspace.MoveItemSpec) {
	return func(s *workspace.MoveItemSpec) { s.NewName = n }
}
func withCompat() func(*workspace.MoveItemSpec) {
	return func(s *workspace.MoveItemSpec) { s.KeepCompat = true }
}

func TestMoveBasicKeepsMeaning(t *testing.T) {
	ws, uA, uB, uC := openABC(t)
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{moveSpec(uA, moveASrc, "add")})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if plan.Issues.Blocking() {
		t.Fatalf("unexpected blocking issues: %+v", plan.Issues)
	}
	item := plan.Items[0]
	if item.Name != "add" || item.NewName != "add" || item.From != uA {
		t.Fatalf("item wrong: %+v", item)
	}
	// One cross-file reference in b, unchanged because the name is kept.
	if len(item.References) != 1 || item.References[0].Location.URI != uB ||
		item.References[0].Decision != workspace.MoveDecisionUnchanged {
		t.Fatalf("references wrong: %+v", item.References)
	}
	// Export changes: a loses add, c gains it.
	var expA, expC *workspace.MoveExportChange
	for i := range plan.Exports {
		switch plan.Exports[i].URI {
		case uA:
			expA = &plan.Exports[i]
		case uC:
			expC = &plan.Exports[i]
		}
	}
	if expA == nil || len(expA.Removed) != 1 || expA.Removed[0] != "add" ||
		expC == nil || len(expC.Added) != 1 || expC.Added[0] != "add" {
		t.Fatalf("exports wrong: %+v", plan.Exports)
	}
	// Import relation after the move: b -> c (add), a -> c (top is separate),
	// and a -> c for add's body using top? top stays in a, so c -> a is new.
	var sawNewEdge bool
	for _, e := range plan.Imports {
		if e.From == uC && e.To == uA && e.New {
			sawNewEdge = true
		}
	}
	if !sawNewEdge {
		t.Fatalf("expected new import edge c->a, got %+v", plan.Imports)
	}

	res, err := ws.ApplyMove(plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.UndoToken == 0 {
		t.Fatalf("missing undo token")
	}
	// Definition of add in b must now jump into c.
	def, err := ws.Definition(uB, strings.Index(moveBSrc, "add(top"))
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if def.Location == nil || def.Location.URI != uC || def.Location.Range.Text != "add" {
		t.Fatalf("add should be declared in c now: %+v", def)
	}
	for _, u := range []snapshot.URI{uA, uB, uC} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %+v", u, diags)
		}
	}
}

func TestMoveRenameWithCompatEntry(t *testing.T) {
	ws, uA, uB, uC := openABC(t)
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{
		moveSpec(uA, moveASrc, "top", withNewName("topV2"), withCompat()),
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if plan.Issues.Blocking() {
		t.Fatalf("blocking issues: %+v", plan.Issues)
	}
	item := plan.Items[0]
	if item.Compat == nil || !strings.Contains(item.Compat.Text, "let top = topV2") {
		t.Fatalf("compat entry wrong: %+v", item.Compat)
	}
	// Both references (a's add body, b's main) default to switch.
	if len(item.References) != 2 {
		t.Fatalf("expected 2 references, got %+v", item.References)
	}
	for _, r := range item.References {
		if r.Decision != workspace.MoveDecisionSwitch {
			t.Fatalf("reference should switch: %+v", r)
		}
	}
	// Partial compatibility: keep the user in b on the old entry.
	for i := range plan.Items[0].References {
		if plan.Items[0].References[i].Location.URI == uB {
			plan.Items[0].References[i].Decision = workspace.MoveDecisionKeep
		}
	}
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// b still uses top, resolved through the compat entry in a.
	def, err := ws.Definition(uB, strings.Index(moveBSrc, "top,"))
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if def.SymbolName != "top" || def.Location == nil || def.Location.URI != uA {
		t.Fatalf("top in b should resolve to compat entry in a: %+v", def)
	}
	// a's add body was switched to topV2, whose declaration lives in c.
	// The compat use of topV2 in a resolves to the moved declaration in c.
	expA := "\nfn add(a, b) {\n  return a + b + topV2\n}\nlet top = topV2\n"
	defV2, err := ws.Definition(uA, strings.Index(expA, "topV2\n"))
	if err != nil {
		t.Fatalf("definition of compat use: %v", err)
	}
	if defV2.SymbolName != "topV2" || defV2.Location == nil || defV2.Location.URI != uC {
		t.Fatalf("topV2 should be declared in c: %+v", defV2)
	}
	for _, u := range []snapshot.URI{uA, uB, uC} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %+v", u, diags)
		}
	}
}

func TestMoveForcedKeepWhenNewNameShadowed(t *testing.T) {
	ws, uA, _, uC := openABC(t)
	// c has a local topV2 shadowing the new exported name.
	cShadow := "fn user() {\n  let topV2 = 5\n  return top + topV2\n}\n"
	ws.Open(uC, cShadow, 2, true)
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{
		moveSpec(uA, moveASrc, "top", withNewName("topV2")),
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if plan.Issues.Blocking() {
		t.Fatalf("blocking issues: %+v", plan.Issues)
	}
	item := plan.Items[0]
	// The reference inside c's user() cannot switch: topV2 is a local there.
	var forced *workspace.MoveReference
	for i := range item.References {
		r := &item.References[i]
		if r.Location.URI == uC {
			forced = r
		}
	}
	if forced == nil || forced.Decision != workspace.MoveDecisionKeep || !forced.Forced {
		t.Fatalf("expected forced keep in c: %+v", item.References)
	}
	// Compat entry auto-enabled because of the forced keep.
	if !item.KeepCompat || item.Compat == nil {
		t.Fatalf("compat should be auto-enabled: %+v", item)
	}
	// The shadowing local is shown as untouched.
	if len(item.Shadowed) == 0 {
		t.Fatalf("expected shadowed occurrences")
	}
	// Overriding the forced decision must be rejected and change nothing.
	bad, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{
		moveSpec(uA, moveASrc, "top", withNewName("topV2")),
	})
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	for i := range bad.Items[0].References {
		bad.Items[0].References[i].Decision = workspace.MoveDecisionSwitch
	}
	if _, err := ws.ApplyMove(bad); err == nil {
		t.Fatalf("overriding a forced keep must fail")
	}
	// Applying the original plan keeps c untouched and clean.
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	def, err := ws.Definition(uC, strings.Index(cShadow, "top +")+0)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if def.SymbolName != "top" || def.Location.URI != uA {
		t.Fatalf("kept reference should resolve via compat in a: %+v", def)
	}
	for _, u := range []snapshot.URI{uA, uC} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %+v", u, diags)
		}
	}
}

func TestMoveConflictRejectedAtomically(t *testing.T) {
	ws, uA, uB, uC := openABC(t)
	// "main" is already a project-global declared in b.
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{
		moveSpec(uA, moveASrc, "add", withNewName("main")),
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(plan.Issues.Conflicts) == 0 {
		t.Fatalf("expected a name conflict, got %+v", plan.Issues)
	}
	if _, err := ws.ApplyMove(plan); err == nil {
		t.Fatalf("apply with blocking conflicts must fail")
	}
	// Original state untouched: add still declared in a.
	def, err := ws.Definition(uB, strings.Index(moveBSrc, "add(top"))
	if err != nil || def.Location.URI != uA {
		t.Fatalf("failed apply must keep original ownership: %+v, %v", def, err)
	}
}

func TestMoveStaleAndUnrelatedEdits(t *testing.T) {
	ws, uA, uB, uC := openABC(t)
	ws.Open("file:///d.edu", "fn other() { return 1 }\n", 1, true)

	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{moveSpec(uA, moveASrc, "add")})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// An unrelated confirmed edit must not invalidate the plan.
	if _, err := ws.ApplyEdit("file:///d.edu", "fn other() { return 2 }\n", 2); err != nil {
		t.Fatalf("unrelated edit: %v", err)
	}
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("apply after unrelated edit should succeed: %v", err)
	}

	// A change to a touched document forces re-confirmation.
	plan2, err := ws.PrepareMove(uA, []workspace.MoveItemSpec{
		{URI: uC, Offset: strings.Index("fn helper() {\n  return 1\n}\nfn add(a, b) {\n  return a + b + top\n}\n", "add(a")},
	})
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	if _, err := ws.ApplyEdit(uA, "let top = 10\n\n", 3); err != nil {
		t.Fatalf("edit a: %v", err)
	}
	if _, err := ws.ApplyMove(plan2); !errors.Is(err, workspace.ErrStaleMovePlan) {
		t.Fatalf("expected stale plan error, got %v", err)
	}
	// Nothing changed: add is still in c, b still references it.
	def, err := ws.Definition(uB, strings.Index(moveBSrc, "add(top"))
	if err != nil || def.Location.URI != uC {
		t.Fatalf("stale apply must not modify anything: %+v, %v", def, err)
	}
}

func TestMoveUndoRestoresOwnershipAndMeaning(t *testing.T) {
	ws, uA, uB, uC := openABC(t)
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{
		moveSpec(uA, moveASrc, "top", withNewName("topV2"), withCompat()),
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	res, err := ws.ApplyMove(plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := ws.UndoMove(res.UndoToken); err != nil {
		t.Fatalf("undo: %v", err)
	}
	// Original ownership and meaning: top declared in a, referenced from b.
	def, err := ws.Definition(uB, strings.Index(moveBSrc, "top,"))
	if err != nil || def.SymbolName != "top" || def.Location.URI != uA {
		t.Fatalf("undo must restore original declaration: %+v, %v", def, err)
	}
	if def.Location.Range.Text != "top" {
		t.Fatalf("declaration text after undo: %q", def.Location.Range.Text)
	}
	// Saved flags restored too: undo brings back the pre-move saved state.
	for _, d := range ws.Status().Documents {
		if (d.URI == uA || d.URI == uC) && !d.Saved {
			t.Fatalf("%s should be saved again after undo", d.URI)
		}
	}
	// A second undo with the same token fails.
	if _, err := ws.UndoMove(res.UndoToken); err == nil {
		t.Fatalf("undo token must be single-use")
	}
}

func TestMoveUndoStaleAfterTouch(t *testing.T) {
	ws, uA, _, uC := openABC(t)
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{moveSpec(uA, moveASrc, "add")})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	res, err := ws.ApplyMove(plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := ws.ApplyEdit(uA, moveASrc+"\n", 3); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := ws.UndoMove(res.UndoToken); !errors.Is(err, workspace.ErrStaleMovePlan) {
		t.Fatalf("undo after touching a moved document must be stale, got %v", err)
	}
}

func TestMoveReportsNewCycle(t *testing.T) {
	ws := workspace.New()
	uA, uB, uC := snapshot.URI("file:///a.edu"), snapshot.URI("file:///b.edu"), snapshot.URI("file:///c.edu")
	srcA := "fn fa() {\n  return gb()\n}\n"
	srcB := "fn gb() {\n  return 1\n}\n"
	srcC := "fn gc() {\n  return fa()\n}\n"
	ws.Open(uA, srcA, 1, true)
	ws.Open(uB, srcB, 1, true)
	ws.Open(uC, srcC, 1, true)
	// Moving gb from b to c makes a<->c mutually dependent.
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{
		{URI: uB, Offset: strings.Index(srcB, "gb")},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(plan.Issues.Cycles) != 1 {
		t.Fatalf("expected one new cycle, got %+v", plan.Issues.Cycles)
	}
	msg := plan.Issues.Cycles[0].Message
	if !strings.Contains(msg, "a.edu") || !strings.Contains(msg, "c.edu") {
		t.Fatalf("cycle should involve a and c: %q", msg)
	}
	// Cycles are informational: apply is still allowed.
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("cycles must not block apply: %v", err)
	}
	for _, u := range []snapshot.URI{uA, uB, uC} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %+v", u, diags)
		}
	}
}

func TestMoveUnresolvedWhenFileUnparseable(t *testing.T) {
	ws, uA, _, uC := openABC(t)
	ws.Open("file:///broken.edu", "fn broken(", 1, false)
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{moveSpec(uA, moveASrc, "add")})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(plan.Issues.Unresolved) == 0 {
		t.Fatalf("expected unresolved references for the broken file")
	}
	if _, err := ws.ApplyMove(plan); err == nil {
		t.Fatalf("apply with unresolved references must fail")
	}
	// Narrowing the scope is not even needed once the file parses again.
	if _, err := ws.ApplyEdit("file:///broken.edu", "fn broken() { return 0 }\n", 2); err != nil {
		t.Fatalf("fix broken: %v", err)
	}
	plan2, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{moveSpec(uA, moveASrc, "add")})
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	if plan2.Issues.Blocking() {
		t.Fatalf("issues should be gone after fixing the file: %+v", plan2.Issues)
	}
}

func TestMoveMultipleDeclarationsTogether(t *testing.T) {
	ws, uA, uB, uC := openABC(t)
	plan, err := ws.PrepareMove(uC, []workspace.MoveItemSpec{
		moveSpec(uA, moveASrc, "add"),
		moveSpec(uA, moveASrc, "top"),
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if plan.Issues.Blocking() {
		t.Fatalf("blocking issues: %+v", plan.Issues)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("expected 2 items")
	}
	if _, err := ws.ApplyMove(plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Both declarations now live in c; b's uses resolve there.
	def, err := ws.Definition(uB, strings.Index(moveBSrc, "add(top"))
	if err != nil || def.Location.URI != uC {
		t.Fatalf("add should be in c: %+v, %v", def, err)
	}
	def2, err := ws.Definition(uB, strings.Index(moveBSrc, "top,"))
	if err != nil || def2.Location.URI != uC {
		t.Fatalf("top should be in c: %+v, %v", def2, err)
	}
	for _, u := range []snapshot.URI{uA, uB, uC} {
		if diags := ws.Diagnostics(u); len(diags) != 0 {
			t.Fatalf("diagnostics in %s: %+v", u, diags)
		}
	}
}
