package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nav/service"
)

func postJSON(t *testing.T, h http.Handler, path string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s -> %d: %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHTTPEndToEnd(t *testing.T) {
	svc := service.New()
	h := New(svc).Handler()

	postJSON(t, h, "/files", map[string]string{"uri": "file:///a.edu", "content": "fn greet(name) { name }\n"})
	postJSON(t, h, "/files", map[string]string{"uri": "file:///b.edu", "content": "fn start() { greet(1) }\n"})

	off := strings.Index("fn start() { greet(1) }\n", "greet")
	req := httptest.NewRequest(http.MethodGet, "/definition?uri=file%3A%2F%2F%2Fb.edu&offset="+itoa(off), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["found"] != true {
		t.Fatalf("definition not found: %s", rec.Body.String())
	}
	result := out["result"].(map[string]any)
	sym := result["symbol"].(map[string]any)
	def := sym["definition"].(map[string]any)
	if def["uri"] != "file:///a.edu" || def["text"] != "greet" {
		t.Fatalf("bad definition: %+v", def)
	}
	if result["snapshot_id"] == "" {
		t.Fatal("result missing snapshot binding")
	}

	// 未保存改动立即生效：把 greet 改名后，b 中的调用变为未解析。
	ch := postJSON(t, h, "/change", map[string]string{
		"uri": "file:///a.edu", "content": "fn greet2(name) { name }\n",
	})
	if ch["version"].(float64) != 1 {
		t.Errorf("unsaved version = %v, want 1", ch["version"])
	}
	req2 := httptest.NewRequest(http.MethodGet, "/references?uri=file%3A%2F%2F%2Fb.edu&offset="+itoa(off), nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	json.Unmarshal(rec2.Body.Bytes(), &out)
	if out["found"] != false {
		t.Fatalf("greet should be unresolved after rename: %s", rec2.Body.String())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
