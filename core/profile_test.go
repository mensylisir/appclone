package core

import (
	"os"
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
