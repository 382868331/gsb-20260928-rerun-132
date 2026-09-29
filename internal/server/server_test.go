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
