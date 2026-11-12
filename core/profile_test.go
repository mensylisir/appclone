package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRawProfile(t *testing.T, e *Env, app string) string {
	t.Helper()
	b, err := os.ReadFile(e.ProfilePath(app))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	return string(b)
}

// `ignored = []` 是用户的显式配置（"不要默认忽略表"），与"没写 ignored"
// （nil，用默认表）语义不同。save → load 必须把两者分开保留，否则一次
// 重新探测就会把显式空表静默换成默认值（§5.5）。
func TestProfileIgnoredEmptyRoundTrip(t *testing.T) {
	e := testEnv(t)
	p := Profile{
		App:         "testapp",
		Exec:        "/opt/app",
		Paths:       []string{"~/.config/App"},
		ProbeMethod: "overlay",
		Ignored:     []string{},
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw := readRawProfile(t, e, "testapp")
	if !strings.Contains(raw, "ignored = []") {
		t.Fatalf("显式空表必须写出来，实际档案：\n%s", raw)
	}
	got, err := e.LoadProfile("testapp")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Ignored == nil {
		t.Fatal("ignored = [] 重新加载后不应变成 nil（会被当成「没声明」）")
	}
	if len(got.Ignored) != 0 {
		t.Fatalf("ignored 应为空表，得到 %v", got.Ignored)
	}
}

// 没声明 ignored 时档案里不写这个键，加载回来仍是 nil（调用方用默认表）。
func TestProfileIgnoredNilRoundTrip(t *testing.T) {
	e := testEnv(t)
	p := Profile{
		App:         "testapp",
		Exec:        "/opt/app",
		Paths:       []string{"~/.config/App"},
		ProbeMethod: "overlay",
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw := readRawProfile(t, e, "testapp")
	if strings.Contains(raw, "ignored") {
		t.Fatalf("nil ignored 不应落盘，实际档案：\n%s", raw)
	}
	got, err := e.LoadProfile("testapp")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Ignored != nil {
		t.Fatalf("没写 ignored 应读回 nil，得到 %v", got.Ignored)
	}
}

// 非空 ignored 当然要原样往返（回归保护）。
func TestProfileIgnoredValuesRoundTrip(t *testing.T) {
	e := testEnv(t)
	p := Profile{
		App:         "testapp",
		Exec:        "/opt/app",
		Paths:       []string{"~/.config/App"},
		ProbeMethod: "overlay",
		Ignored:     []string{"~/.cache", "~/.config/App/Cache"},
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := e.LoadProfile("testapp")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Ignored) != 2 || got.Ignored[0] != "~/.cache" || got.Ignored[1] != "~/.config/App/Cache" {
		t.Fatalf("ignored 未原样往返：%v", got.Ignored)
	}
}

// read_trace / credential_paths 必须双入口（SaveProfile 与 encodeProfile）往返，
// 且旧档案缺键时读回 ""（未知）而不是猜成 ok。
func TestProfileReadTraceCredentialRoundTrip(t *testing.T) {
	e := testEnv(t)
	p := Profile{
		App:             "testapp",
		Exec:            "/opt/app",
		Paths:           []string{"~/.zcode"},
		CredentialPaths: []string{"~/.zcode"},
		ReadTrace:       "ok",
		ProbeMethod:     "overlay",
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw := readRawProfile(t, e, "testapp")
	if !strings.Contains(raw, `read_trace = "ok"`) || !strings.Contains(raw, `credential_paths = ["~/.zcode"]`) {
		t.Fatalf("新字段没落盘：\n%s", raw)
	}
	got, err := e.LoadProfile("testapp")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ReadTrace != "ok" || len(got.CredentialPaths) != 1 || got.CredentialPaths[0] != "~/.zcode" {
		t.Fatalf("往返丢字段：read_trace=%q credential_paths=%v", got.ReadTrace, got.CredentialPaths)
	}

	b, err := testEnv(t).encodeProfile(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(b), "read_trace") || !strings.Contains(string(b), "credential_paths") {
		t.Fatalf("encodeProfile 丢新字段：\n%s", b)
	}
}

func TestProfileReadTraceLegacyEmpty(t *testing.T) {
	e := testEnv(t)
	raw := "version = 1\nexec = \"/opt/app\"\npaths = [\"~/.config/App\"]\nprobe_method = \"overlay\"\n"
	if err := os.MkdirAll(filepath.Dir(e.ProfilePath("old")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.ProfilePath("old"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadProfile("old")
	if err != nil {
		t.Fatalf("旧档案应可加载：%v", err)
	}
	if p.ReadTrace != "" || len(p.CredentialPaths) != 0 {
		t.Fatalf("旧档案缺键应读成空值：%q %v", p.ReadTrace, p.CredentialPaths)
	}
}

func TestProfileStringShowsReadEvidence(t *testing.T) {
	p := Profile{Exec: "/opt/app", Paths: []string{"~/.zcode"}, ReadTrace: "partial", CredentialPaths: []string{"~/.zcode"}}
	s := p.String()
	if !strings.Contains(s, "read_trace=\"partial\"") || !strings.Contains(s, "credential_paths=[~/.zcode]") {
		t.Fatalf("String 必须同时显示 read_trace 与凭据路径：%s", s)
	}
}
