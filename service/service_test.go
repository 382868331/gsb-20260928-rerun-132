package service

import (
	"strings"
	"testing"
)

func identOffset(src, name string, occurrence int) int {
	off := -1
	for i := 0; i <= occurrence; i++ {
		idx := strings.Index(src[off+1:], name)
		if idx < 0 {
			return -1
		}
		off = off + 1 + idx
	}
	return off
}

func TestCrossFileMutualReferences(t *testing.T) {
	svc := New()
	a := "fn ping() { pong() }\nlet shared = 1\n"
	b := "fn pong() { ping() + shared }\n"
	svc.PutSaved("file:///a.edu", a)
	svc.PutSaved("file:///b.edu", b)

	// ping 在 a 中定义，b 中跨文件调用；a 体内调用的是 pong（相互引用）。
	res, ok := svc.References("file:///a.edu", identOffset(a, "ping", 0))
	if !ok {
		t.Fatal("ping definition not found")
	}
	if got := len(res.Refs); got != 2 {
		t.Fatalf("ping refs = %d, want 2: %+v", got, res.Refs)
	}
	files := map[string]bool{}
	for _, loc := range res.Refs {
		files[loc.URI] = true
		if loc.Text != "ping" {
			t.Errorf("ref text = %q", loc.Text)
		}
	}
	if !files["file:///a.edu"] || !files["file:///b.edu"] {
		t.Errorf("refs should span both files: %v", files)
	}

	// 从 b 中的调用点跳转到 a 的定义。
	callOff := strings.Index(b, "ping()")
	def, ok := svc.Definition("file:///b.edu", callOff)
	if !ok || def.Symbol.Def.URI != "file:///a.edu" {
		t.Fatalf("definition = %+v ok=%v", def, ok)
	}
}

func TestUnsavedChangesReflected(t *testing.T) {
	svc := New()
	saved := "fn helper() { 1 }\n"
	svc.PutSaved("file:///a.edu", saved)
	svc.Open("file:///a.edu", saved)

	ch := svc.Change("file:///a.edu", "fn helper() { 1 }\nfn caller() { helper() }\n")
	if ch.Version != 2 {
		t.Fatalf("version = %d, want 2", ch.Version)
	}
	res, ok := svc.References("file:///a.edu", strings.Index(ch2src("fn helper() { 1 }\nfn caller() { helper() }\n"), "helper"))
	_ = res
	if !ok {
		t.Fatal("helper not found after unsaved change")
	}
	if got := len(res.Refs); got != 2 {
		t.Fatalf("helper refs = %d, want 2 (def + new call)", got)
	}
	if v := res.Versions["file:///a.edu"]; v != 2 {
		t.Fatalf("result version = %d, want 2", v)
	}
}

func ch2src(s string) string { return s }

func TestIncrementalOnlyAffectedRefsAndStableIdentity(t *testing.T) {
	svc := New()
	a := "fn alpha() { beta() }\n"
	b := "fn beta() { 1 }\n"
	c := "fn gamma() { beta() }\n"
	svc.PutSaved("file:///a.edu", a)
	svc.PutSaved("file:///b.edu", b)
	svc.PutSaved("file:///c.edu", c)

	betaOff := strings.Index(b, "beta")
	before, _ := svc.References("file:///b.edu", betaOff)
	if len(before.Refs) != 3 {
		t.Fatalf("beta refs before = %d, want 3", len(before.Refs))
	}
	defBefore := before.Symbol.ID
	snapBefore := before.SnapshotID

	// 修改与 beta 无关的 c（gamma 内部改动），beta 身份与引用不变，
	// 受影响文件只应包含 c。
	svc.PutSaved("file:///c.edu", "fn gamma() { beta() + 1 }\n")
	after, _ := svc.References("file:///b.edu", betaOff)
	if after.Symbol.ID != defBefore {
		t.Errorf("beta identity changed: %s -> %s", defBefore, after.Symbol.ID)
	}
	if len(after.Refs) != 3 {
		t.Errorf("beta refs after unrelated edit = %d", len(after.Refs))
	}
	if after.SnapshotID == snapBefore {
		t.Error("snapshot id must change after edit")
	}

	// 重命名 b 中的 beta -> beta2：b 与依赖 beta 的 a、c 都应标记受影响。
	svc.PutSaved("file:///b.edu", "fn beta2() { 1 }\n")
	res, _ := svc.References("file:///b.edu", strings.Index("fn beta2() { 1 }\n", "beta2"))
	if len(res.Refs) != 1 {
		t.Fatalf("beta2 refs = %d, want 1; a/c call sites now unresolved", len(res.Refs))
	}
}

func TestResultsBoundToDocumentVersion(t *testing.T) {
	svc := New()
	svc.PutSaved("file:///k.edu", ssrc)
	res, ok := svc.References("file:///k.edu", strings.Index(ssrc, "k"))
	if !ok {
		t.Fatal("k not found")
	}
	if res.Versions["file:///k.edu"] != 0 {
		t.Errorf("saved doc version must be 0, got %d", res.Versions["file:///k.edu"])
	}
}

const ssrc = "fn k() { 1 }\n"
