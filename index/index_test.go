package index

import (
	"testing"

	"nav/snapshot"
)

func buildSnap(files map[string]string) *snapshot.Snapshot {
	st := snapshot.NewStore()
	for uri, content := range files {
		st.PutSaved(uri, content)
	}
	return st.Build()
}

func TestIncrementalAffectedSet(t *testing.T) {
	p := New()
	files := map[string]string{
		"a": "fn alpha() { beta() }\n",
		"b": "fn beta() { 1 }\n",
		"c": "fn gamma() { 1 }\n",
	}
	p.Update(buildSnap(files))

	// 修改与全局 beta 无关的 c：仅 c 受影响。
	files["c"] = "fn gamma() { 2 }\n"
	affected := p.Update(buildSnap(files))
	if len(affected) != 1 || !contains(affected, "c") {
		t.Fatalf("unrelated edit affected %v", affected)
	}

	// 在 c 中新增对 beta 的依赖：c 受影响，beta 引用增加。
	files["c"] = "fn gamma() { beta() }\n"
	affected = p.Update(buildSnap(files))
	if len(affected) != 1 || !contains(affected, "c") {
		t.Fatalf("new dependency affected %v", affected)
	}
	if n := len(p.refs[id("beta")]); n != 3 {
		t.Fatalf("beta refs = %d, want 3", n)
	}

	// b 中删除 beta：b 与其依赖文件 a、c 都需重绑定。
	files["b"] = "fn other() { 1 }\n"
	affected = p.Update(buildSnap(files))
	if len(affected) != 3 || !contains(affected, "a") || !contains(affected, "b") || !contains(affected, "c") {
		t.Fatalf("global removal affected %v", affected)
	}
}

func contains(m map[string]struct{}, k string) bool {
	_, ok := m[k]
	return ok
}
