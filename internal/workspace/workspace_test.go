package workspace_test

import (
	"strings"
	"testing"

	"sourcenav/internal/snapshot"
	"sourcenav/internal/workspace"
)

// Two files reference each other's top-level symbols in both directions.
const (
	aSrc = `fn alpha(x) {
  return beta(x) + 1
}

let shared = 42
`
	bSrc = `fn beta(n) {
  return alpha(n) + shared
}
`
)

func openPair(t *testing.T) (*workspace.Workspace, snapshot.URI, snapshot.URI) {
	t.Helper()
	ws := workspace.New()
	uA := snapshot.URI("file:///a.edu")
	uB := snapshot.URI("file:///b.edu")
	ws.Open(uA, aSrc, 1, true)
	ws.Open(uB, bSrc, 1, true)
	return ws, uA, uB
}

func TestCrossFileMutualReferences(t *testing.T) {
	ws, uA, uB := openPair(t)

	// "beta(x)" inside alpha — offset sits on the identifier.
	off := strings.Index(aSrc, "beta(x)")
	def, err := ws.Definition(uA, off)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if def.Location == nil {
		t.Fatal("expected definition location for beta")
	}
	if def.Location.URI != uB {
		t.Fatalf("beta defined in %s, want %s", def.Location.URI, uB)
	}
	if def.Location.Range.Text != "beta" {
		t.Fatalf("definition text = %q", def.Location.Range.Text)
	}
	if def.Location.Version != 1 {
		t.Fatalf("definition not bound to version 1: %d", def.Location.Version)
	}

	refs, err := ws.References(uA, off)
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	// beta: 1 declaration in b + 1 use in a.
	var decls, uses int
	for _, l := range refs.References {
		if l.Role == "reference" {
			uses++
		}
		if l.URI == uA && l.Range.Text != "beta" {
			t.Fatalf("reference text not real source: %q", l.Range.Text)
		}
	}
	if refs.Declaration == nil || refs.Declaration.URI != uB {
		t.Fatal("beta declaration missing or wrong file")
	}
	decls = 1
	if uses != 1 || decls != 1 {
		t.Fatalf("beta refs: uses=%d decls=%d", uses, decls)
	}

	// shared is declared in a and read in b: cross-file variable reference.
	offShared := strings.Index(bSrc, "shared")
	defShared, err := ws.Definition(uB, offShared)
	if err != nil {
		t.Fatalf("shared definition: %v", err)
	}
	if defShared.Location == nil || defShared.Location.URI != uA {
		t.Fatalf("shared should resolve into %s", uA)
	}
}

// A nested local shadows an outer one and a file-level one; same-named locals
// in separate scopes must get distinct symbols.
const shadowSrc = `let v = 1

fn outer(v) {
  if v > 0 {
    let v = 9
    return v
  }
  return v
}

fn other() {
  return v
}
`

func TestNestedScopesShadowing(t *testing.T) {
	ws := workspace.New()
	u := snapshot.URI("file:///shadow.edu")
	ws.Open(u, shadowSrc, 1, true)

	innerUse := indexNth(shadowSrc, "v", 5) // "return v" inside the if block
	refsInner, err := ws.References(u, innerUse)
	if err != nil {
		t.Fatalf("inner refs: %v", err)
	}
	// Inner local v: declaration "let v = 9" + one use in the same if block.
	if refsInner.SymbolName != "v" {
		t.Fatalf("symbol name = %q", refsInner.SymbolName)
	}
	count := len(refsInner.References)
	if refsInner.Declaration != nil {
		count++
	}
	if count != 2 {
		t.Fatalf("inner v should have 2 occurrences, got %d", count)
	}
	if refsInner.Declaration == nil || !strings.Contains(refsInner.Declaration.Range.Text, "v") {
		t.Fatal("inner declaration missing")
	}

	// The parameter v in "return v" after the if resolves to the parameter,
	// not to the inner let nor the global v.
	paramUse := indexNth(shadowSrc, "return v", 2) + len("return ")
	defParam, err := ws.Definition(u, paramUse)
	if err != nil {
		t.Fatalf("param definition: %v", err)
	}
	if defParam.Location == nil {
		t.Fatal("parameter v has no declaration")
	}
	paramDeclLine := defParam.Location.Range.StartLine
	if paramDeclLine != 3 { // fn outer(v) is line 3
		t.Fatalf("parameter v declared on line %d, want 3", paramDeclLine)
	}

	// other() uses the file-level v, never outer's parameter or the inner let.
	otherUse := indexNth(shadowSrc, "return v", 3) + len("return ")
	defGlobal, err := ws.Definition(u, otherUse)
	if err != nil {
		t.Fatalf("global definition: %v", err)
	}
	if defParam.SymbolID == defGlobal.SymbolID {
		t.Fatal("parameter v and global v share identity")
	}
	if defGlobal.Location == nil || defGlobal.Location.Range.StartLine != 1 {
		t.Fatalf("other() v must bind to line-1 global")
	}
}

func TestUnsavedEditReflectedAndIncremental(t *testing.T) {
	ws, uA, uB := openPair(t)

	// Unsaved change in a: rename alpha -> alpha2 locally, still calling beta.
	newA := strings.Replace(aSrc, "fn alpha(", "fn alpha2(", 1)
	vi := ws.Open(uA, newA, 2, false)
	if vi.SnapshotID == 0 || vi.Documents[string(uA)] != 2 || vi.Documents[string(uB)] != 1 {
		t.Fatalf("version binding wrong: %+v", vi)
	}

	// Unsaved change in b as well: its use now names alpha2 (mutual
	// reference), so it must jump to the unsaved declaration in a.
	newB := strings.Replace(bSrc, "alpha(n)", "alpha2(n)", 1)
	ws.Open(uB, newB, 2, false)
	off := strings.Index(newB, "alpha2(n)")
	def, err := ws.Definition(uB, off)
	if err != nil {
		t.Fatalf("definition after edit: %v", err)
	}
	if def.Location == nil || def.Location.URI != uA || def.Location.Version != 2 {
		t.Fatalf("alpha should resolve to unsaved v2 in a: %+v", def.Location)
	}
	if def.SymbolName != "alpha2" {
		t.Fatalf("resolved symbol = %q want alpha2", def.SymbolName)
	}

	refs, err := ws.References(uB, off)
	if err != nil {
		t.Fatalf("refs after edit: %v", err)
	}
	// alpha2: declaration in a + use in b. No stale alpha references remain.
	seen := map[string]int{}
	if refs.Declaration != nil {
		seen[string(refs.Declaration.URI)]++
	}
	for _, l := range refs.References {
		seen[string(l.URI)]++
		if l.Range.Text != "alpha2" {
			t.Fatalf("stale text %q in references", l.Range.Text)
		}
	}
	if seen[string(uA)] != 1 || seen[string(uB)] != 1 {
		t.Fatalf("alpha2 occurrence files wrong: %+v", seen)
	}
}

func TestUnaffectedReferencesStayStable(t *testing.T) {
	ws, uA, uB := openPair(t)

	// Add an unrelated function to a; references to beta must keep identity
	// and still include b's declaration, with only the affected files
	// re-analyzed (semantically, behavior is identical).
	before, err := ws.References(uA, strings.Index(aSrc, "beta(x)"))
	if err != nil {
		t.Fatal(err)
	}
	newA := aSrc + "fn unrelated() { return beta() }\n"
	ws.Open(uA, newA, 2, false)
	after, err := ws.References(uA, strings.Index(newA, "beta(x)"))
	if err != nil {
		t.Fatal(err)
	}
	if before.SymbolID != after.SymbolID {
		t.Fatal("beta symbol identity changed across unrelated edit")
	}
	if after.Declaration == nil || after.Declaration.URI != uB {
		t.Fatal("beta declaration lost after unrelated edit")
	}
}

func TestRangesAreRealSource(t *testing.T) {
	ws, uA, _ := openPair(t)
	off := strings.Index(aSrc, "shared")
	refs, err := ws.References(uA, off)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Declaration == nil {
		t.Fatal("shared declaration missing")
	}
	l := refs.Declaration.Range
	got := aSrc[l.Start:l.End]
	if got != "shared" {
		t.Fatalf("range slices %q, want shared", got)
	}
	if l.StartLine != 5 || l.StartColumn != 5 {
		t.Fatalf("line/col wrong: %+v", l.Range)
	}
}

func TestUndefinedNameDiagnostic(t *testing.T) {
	ws := workspace.New()
	u := snapshot.URI("file:///d.edu")
	ws.Open(u, "fn f() { return nope() }\n", 1, true)
	diags := ws.Diagnostics(u)
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "undeclared") && d.Message != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected undeclared diagnostic, got %+v", diags)
	}
}

func indexNth(s, sub string, n int) int { return indexNthFrom(s, sub, 0, n) }

func indexNthFrom(s, sub string, from, n int) int {
	i := from
	for k := 0; k < n; k++ {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			return -1
		}
		i += j
		if k == n-1 {
			return i
		}
		i += len(sub)
	}
	return -1
}
