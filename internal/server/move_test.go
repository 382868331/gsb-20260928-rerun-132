package server_test

import (
	"net/http"
	"strings"
	"testing"

	"sourcenav/internal/server"
	"sourcenav/internal/workspace"
)

func TestHTTPMoveEndToEnd(t *testing.T) {
	h := server.New(workspace.New()).Mux()

	srcA := "let top = 10\n\nfn add(a, b) {\n  return a + b + top\n}\n"
	srcB := "fn main() {\n  return add(top, 2)\n}\n"
	srcC := "fn helper() {\n  return 1\n}\n"
	for _, d := range []struct{ uri, text string }{
		{"file:///a.edu", srcA}, {"file:///b.edu", srcB}, {"file:///c.edu", srcC},
	} {
		if code, _ := post(t, h, "/open", map[string]any{
			"uri": d.uri, "text": d.text, "version": 1, "saved": true,
		}); code != http.StatusOK {
			t.Fatalf("open %s status %d", d.uri, code)
		}
	}

	// Preview moving add from a to c.
	code, plan := post(t, h, "/move/prepare", map[string]any{
		"target": "file:///c.edu",
		"items": []map[string]any{
			{"uri": "file:///a.edu", "offset": strings.Index(srcA, "add")},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("move/prepare status %d: %v", code, plan)
	}
	items, _ := plan["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item: %v", plan)
	}
	item, _ := items[0].(map[string]any)
	if item["name"] != "add" || item["from"] != "file:///a.edu" {
		t.Fatalf("item wrong: %v", item)
	}
	refs, _ := item["references"].([]any)
	if len(refs) != 1 {
		t.Fatalf("expected 1 cross-file reference: %v", item)
	}
	exports, _ := plan["exports"].([]any)
	if len(exports) != 2 {
		t.Fatalf("expected export changes for a and c: %v", plan["exports"])
	}

	// Apply the confirmed plan.
	code, applied := post(t, h, "/move/apply", plan)
	if code != http.StatusOK {
		t.Fatalf("move/apply status %d: %v", code, applied)
	}
	token, _ := applied["undoToken"].(float64)
	if token == 0 {
		t.Fatalf("missing undo token: %v", applied)
	}
	code, def := post(t, h, "/definition", map[string]any{
		"uri": "file:///b.edu", "offset": strings.Index(srcB, "add(top"),
	})
	if code != http.StatusOK {
		t.Fatalf("definition status %d", code)
	}
	loc, _ := def["location"].(map[string]any)
	if loc["uri"] != "file:///c.edu" {
		t.Fatalf("add should be declared in c after move: %v", def)
	}

	// Undo restores the original declaration ownership.
	code, _ = post(t, h, "/move/undo", map[string]any{"token": token})
	if code != http.StatusOK {
		t.Fatalf("move/undo status %d", code)
	}
	code, def = post(t, h, "/definition", map[string]any{
		"uri": "file:///b.edu", "offset": strings.Index(srcB, "add(top"),
	})
	if code != http.StatusOK {
		t.Fatalf("definition after undo status %d", code)
	}
	loc, _ = def["location"].(map[string]any)
	if loc["uri"] != "file:///a.edu" {
		t.Fatalf("undo should restore declaration in a: %v", def)
	}
}

func TestHTTPMoveApplyStaleConflict(t *testing.T) {
	h := server.New(workspace.New()).Mux()
	srcA := "let top = 10\n\nfn add(a, b) {\n  return a + b + top\n}\n"
	srcC := "fn helper() {\n  return 1\n}\n"
	for _, d := range []struct{ uri, text string }{
		{"file:///a.edu", srcA}, {"file:///c.edu", srcC},
	} {
		if code, _ := post(t, h, "/open", map[string]any{
			"uri": d.uri, "text": d.text, "version": 1, "saved": true,
		}); code != http.StatusOK {
			t.Fatalf("open %s status %d", d.uri, code)
		}
	}
	code, plan := post(t, h, "/move/prepare", map[string]any{
		"target": "file:///c.edu",
		"items": []map[string]any{
			{"uri": "file:///a.edu", "offset": strings.Index(srcA, "add")},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("move/prepare status %d: %v", code, plan)
	}
	// Touch a document the plan writes to, then apply: 409 and no change.
	if code, _ := post(t, h, "/edit", map[string]any{
		"uri": "file:///a.edu", "text": srcA + "\n", "version": 2,
	}); code != http.StatusOK {
		t.Fatalf("edit status %d", code)
	}
	if code, body := post(t, h, "/move/apply", plan); code != http.StatusConflict {
		t.Fatalf("stale apply should be 409, got %d: %v", code, body)
	}
	code, def := post(t, h, "/definition", map[string]any{
		"uri": "file:///a.edu", "offset": strings.Index(srcA, "add"),
	})
	if code != http.StatusOK {
		t.Fatalf("definition status %d", code)
	}
	loc, _ := def["location"].(map[string]any)
	if loc["uri"] != "file:///a.edu" {
		t.Fatalf("failed apply must leave add in a: %v", def)
	}
}
