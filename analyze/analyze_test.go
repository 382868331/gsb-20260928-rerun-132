package analyze

import (
	"strings"
	"testing"
)

const nestedSrc = `
let outer = 1
fn f(x) {
  let y = x + outer
  if y > 0 {
    let x = y + 1
    use(x)
  } else {
    use(x)
  }
}
`

func TestNestedScopesAndShadowing(t *testing.T) {
	res, err := Analyze("f.edu", nestedSrc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	xIDs := map[string]bool{}
	for _, sym := range res.Symbols {
		if sym.Name == "x" {
			xIDs[sym.ID.String()] = true
		}
	}
	if len(xIDs) != 2 {
		t.Fatalf("want param x and shadowed local x, got %d: %v", len(xIDs), xIDs)
	}

	paramUse := strings.Index(nestedSrc, "x + outer")
	firstUseX := strings.Index(nestedSrc, "use(x)") + 4
	secondUseX := strings.LastIndex(nestedSrc, "use(x)") + 4

	xRefs := 0
	for _, rf := range res.Refs {
		if rf.Text != "x" {
			continue
		}
		xRefs++
		want := ""
		switch rf.Start {
		case paramUse:
			want = "f"
		case firstUseX:
			want = "f/if#0"
		case secondUseX:
			want = "f"
		default:
			t.Errorf("unexpected x ref at %d", rf.Start)
		}
		if !rf.Resolved {
			t.Errorf("x ref at %d unresolved", rf.Start)
		}
		if got := rf.Def.Container; got != want {
			t.Errorf("x ref at %d bound to %q, want %q", rf.Start, got, want)
		}
	}
	if xRefs != 3 {
		t.Fatalf("want exactly 3 x references, got %d", xRefs)
	}
}

func TestGlobalRefsDeferred(t *testing.T) {
	res, _ := Analyze("a.edu", "fn a() { b() + g }")
	if _, ok := res.GlobalUses["b"]; !ok {
		t.Fatal("b should be deferred as global")
	}
	if _, ok := res.GlobalUses["g"]; !ok {
		t.Fatal("g should be deferred as global")
	}
	for _, rf := range res.Refs {
		if rf.Resolved || rf.GlobalName == "" {
			t.Errorf("single-file global use must remain unresolved with name, got %+v", rf)
		}
	}
}

func TestRangesReturnRealSource(t *testing.T) {
	src := "let greeting = 42"
	res, _ := Analyze("r.edu", src)
	sym := res.Symbols[0]
	txt, err := mustRange(sym.DefStart, sym.DefEnd).Text(src)
	if err != nil || txt != "greeting" {
		t.Fatalf("range text = %q, err=%v", txt, err)
	}
}
