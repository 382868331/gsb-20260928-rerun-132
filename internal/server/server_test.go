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
func TestHTTPRenamePreviewApplyAndStale(t *testing.T) {
	h := server.New(workspace.New()).Mux()

	srcA := "fn main() { return helper(1) }\n"
	srcB := "fn helper(n) { return n + 1 }\n"
	for _, d := range []map[string]any{
		{"uri": "file:///m.edu", "text": srcA, "version": 1, "saved": true},
		{"uri": "file:///h.edu", "text": srcB, "version": 1, "saved": true},
	} {
		if code, _ := post(t, h, "/open", d); code != http.StatusOK {
			t.Fatalf("open status %d", code)
		}
	}

	// Preview: rename helper -> util across both files.
	code, plan := post(t, h, "/rename/prepare", map[string]any{
		"uri": "file:///m.edu", "offset": strings.Index(srcA, "helper"), "newName": "util",
	})
	if code != http.StatusOK {
		t.Fatalf("prepare status %d: %v", code, plan)
	}
	files, _ := plan["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("plan should touch 2 files: %v", plan)
	}

	// A related document changes after the preview: the old plan must be
	// rejected with 409, not applied at stale offsets.
	if code, _ := post(t, h, "/edit", map[string]any{
		"uri": "file:///h.edu", "text": "// touched\n" + srcB, "version": 2,
	}); code != http.StatusOK {
		t.Fatalf("edit status %d", code)
	}
	if code, _ := post(t, h, "/rename/apply", plan); code != http.StatusConflict {
		t.Fatalf("stale apply status = %d, want 409", code)
	}

	// Re-preview against the current snapshot, then apply.
	code, plan2 := post(t, h, "/rename/prepare", map[string]any{
		"uri": "file:///m.edu", "offset": strings.Index(srcA, "helper"), "newName": "util",
	})
	if code != http.StatusOK {
		t.Fatalf("re-prepare status %d", code)
	}
	code, applied := post(t, h, "/rename/apply", plan2)
	if code != http.StatusOK {
		t.Fatalf("apply status %d: %v", code, applied)
	}
	for _, d := range applied["documents"].([]any) {
		doc := d.(map[string]any)
		if doc["saved"].(bool) {
			t.Fatalf("renamed doc %v must be unsaved (session text only)", doc["uri"])
		}
	}

	// Definition and diagnostics re-checked after the rename.
	code, def := post(t, h, "/definition", map[string]any{
		"uri": "file:///m.edu", "offset": strings.Index(srcA, "helper"),
	})
	if code != http.StatusOK || def["symbolName"] != "util" {
		t.Fatalf("definition after rename: %d %v", code, def)
	}
	loc := def["location"].(map[string]any)
	if loc["uri"] != "file:///h.edu" || loc["range"].(map[string]any)["text"] != "util" {
		t.Fatalf("util declaration wrong: %v", loc)
	}
	for _, u := range []string{"file:///m.edu", "file:///h.edu"} {
		code, diags := post(t, h, "/diagnostics", map[string]any{"uri": u})
		if code != http.StatusOK {
			t.Fatalf("diagnostics status %d", code)
		}
		list, _ := diags["diagnostics"].([]any)
		if len(list) != 0 {
			t.Fatalf("diagnostics in %s after rename: %v", u, list)
		}
	}
}
