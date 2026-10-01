package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sourcenav/internal/server"
	"sourcenav/internal/workspace"
)

func post(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestHTTPEndToEnd(t *testing.T) {
	h := server.New(workspace.New()).Mux()

	src := "fn main() { return helper(1) }\n"
	if code, _ := post(t, h, "/open", map[string]any{
		"uri": "file:///m.edu", "text": src, "version": 1, "saved": true,
	}); code != http.StatusOK {
		t.Fatalf("open status %d", code)
	}
	src2 := "fn helper(n) { return n + 1 }\n"
	if code, _ := post(t, h, "/open", map[string]any{
		"uri": "file:///h.edu", "text": src2, "version": 1, "saved": true,
	}); code != http.StatusOK {
		t.Fatalf("open status %d", code)
	}

	off := strings.Index(src, "helper")
	code, refs := post(t, h, "/references", map[string]any{
		"uri": "file:///m.edu", "offset": off,
	})
	if code != http.StatusOK {
		t.Fatalf("references status %d", code)
	}
	decl, _ := refs["declaration"].(map[string]any)
	if decl == nil || decl["uri"] != "file:///h.edu" {
		t.Fatalf("cross-file declaration wrong: %v", decl)
	}
	rng, _ := decl["range"].(map[string]any)
	if rng["text"] != "helper" {
		t.Fatalf("range text = %v", rng["text"])
	}
	version, _ := refs["version"].(map[string]any)
	if version["snapshotId"].(float64) < 2 {
		t.Fatalf("snapshot id not bound: %v", version)
	}
}
func TestRenameOverHTTP(t *testing.T) {
	h := server.New(workspace.New()).Mux()

	a := "fn alpha(x) { return beta(x) + 1 }\n"
	b := "fn beta(n) { return alpha(n) }\n"
	for _, d := range []map[string]any{
		{"uri": "file:///a.edu", "text": a, "version": 1, "saved": true},
		{"uri": "file:///b.edu", "text": b, "version": 1, "saved": true},
	} {
		if code, _ := post(t, h, "/open", d); code != http.StatusOK {
			t.Fatalf("open status %d", code)
		}
	}

	off := strings.Index(b, "alpha")
	code, plan := post(t, h, "/rename/prepare", map[string]any{
		"uri": "file:///b.edu", "offset": off, "newName": "first",
	})
	if code != http.StatusOK {
		t.Fatalf("prepare status %d: %v", code, plan)
	}
	edits, _ := plan["edits"].([]any)
	if len(edits) != 2 {
		t.Fatalf("edits = %v", plan["edits"])
	}
	planID := plan["planId"]

	// A related document changes after the preview: the plan must be
	// rejected with 409 instead of writing stale offsets.
	if code, _ := post(t, h, "/edit", map[string]any{
		"uri": "file:///a.edu", "text": "// shift\n" + a, "version": 2,
	}); code != http.StatusOK {
		t.Fatalf("edit status %d", code)
	}
	code, body := post(t, h, "/rename/apply", map[string]any{"planId": planID, "save": false})
	if code != http.StatusConflict {
		t.Fatalf("stale apply status %d, want 409: %v", code, body)
	}

	// Re-preview on the new text, then apply with save=true.
	code, plan2 := post(t, h, "/rename/prepare", map[string]any{
		"uri": "file:///b.edu", "offset": off, "newName": "first",
	})
	if code != http.StatusOK {
		t.Fatalf("re-prepare status %d: %v", code, plan2)
	}
	code, applied := post(t, h, "/rename/apply", map[string]any{"planId": plan2["planId"], "save": true})
	if code != http.StatusOK {
		t.Fatalf("apply status %d: %v", code, applied)
	}
	if applied["appliedEdits"].(float64) != 2 || applied["saved"] != true {
		t.Fatalf("apply result = %v", applied)
	}

	// Definition on the renamed symbol still resolves cross-file.
	code, def := post(t, h, "/definition", map[string]any{"uri": "file:///b.edu", "offset": off})
	if code != http.StatusOK || def["symbolName"] != "first" {
		t.Fatalf("definition after rename: %d %v", code, def)
	}
	loc, _ := def["location"].(map[string]any)
	if loc == nil || loc["uri"] != "file:///a.edu" {
		t.Fatalf("definition location = %v", loc)
	}
}
