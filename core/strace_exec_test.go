package core

import (
	"strings"
	"testing"
)

func parseExecTraceLine(t *testing.T, line string) StraceResult {
	t.Helper()
	return ParseStraceAll(strings.NewReader(line+"\n"), "/home/u")
}

func TestQuotedArray(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		ok   bool
	}{
		{"空数组", "[]", []string{}, true},
		{"普通", `["a", "b"]`, []string{"a", "b"}, true},
		{"逗号后无空格", `["a","b"]`, []string{"a", "b"}, true},
		{"空格与中文", `["/opt/Qoder CN/qoder-cn", "--ozone-platform=x11"]`,
			[]string{"/opt/Qoder CN/qoder-cn", "--ozone-platform=x11"}, true},
		{"转义引号", `["a\"b"]`, []string{`a"b`}, true},
		{"反斜杠", `["a\\b"]`, []string{`a\b`}, true},
		{"空字符串元素", `[""]`, []string{""}, true},
		{"截断", `["a"...`, nil, false},
		{"引号外垃圾", `["a"]x`, nil, false},
		{"非字符串元素", `["a", 1]`, nil, false},
		{"未闭合", `["a`, nil, false},
		{"非法 UTF-8", "[\"\xff\"]", nil, false},
		{"NUL", "[\"a\x00b\"]", nil, false},
		{"超长", `["` + strings.Repeat("x", 513) + `"]`, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := quotedArray(c.in)
			if ok != c.ok {
				t.Fatalf("ok=%v，期望 %v（in=%q）", ok, c.ok, c.in)
			}
			if !ok {
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %q，期望 %q", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %q，期望 %q", got, c.want)
				}
			}
		})
	}
}

// execve 的路径是 parts[0]、argv 是 parts[1]；execveat 的路径是 parts[1]、
// argv 是其后第一个 `[` 参数。这两条下标一旦写错，证据会静默错位。
func TestParseExecCallIndices(t *testing.T) {
	res := parseExecTraceLine(t, `1234 execve("/opt/Qoder CN/qoder-cn", ["/opt/Qoder CN/qoder-cn", "--flag"], 0x7ffd) = 0`)
	if len(res.ExecCalls) != 1 {
		t.Fatalf("execve 应记录 1 条证据，得到 %#v", res.ExecCalls)
	}
	c := res.ExecCalls[0]
	if c.Path != "/opt/Qoder CN/qoder-cn" || !c.RetKnown || c.Round != 0 {
		t.Fatalf("execve 证据错：%#v", c)
	}
	if len(c.Args) != 2 || c.Args[0] != "/opt/Qoder CN/qoder-cn" || c.Args[1] != "--flag" {
		t.Fatalf("execve argv 错：%q", c.Args)
	}

	res = parseExecTraceLine(t, `1234 execveat(3</opt/app>, "qoder-cn", ["qoder-cn", "--flag"], 0x7ffd, 0) = 0`)
	if len(res.ExecCalls) != 1 {
		t.Fatalf("execveat 应记录 1 条证据，得到 %#v", res.ExecCalls)
	}
	c = res.ExecCalls[0]
	if c.Path != "/opt/app/qoder-cn" {
		t.Fatalf("execveat 路径应取 parts[1] 并解析 dirfd，得到 %q", c.Path)
	}
	if len(c.Args) != 2 || c.Args[1] != "--flag" {
		t.Fatalf("execveat argv 应取第一个 `[` 参数，得到 %q", c.Args)
	}
}

func TestParseExecCallEmptyPath(t *testing.T) {
	res := parseExecTraceLine(t, `1234 execveat(5</opt/app/qoder-cn>, "", ["qoder-cn"], 0x7ffd, AT_EMPTY_PATH) = 0`)
	if len(res.ExecCalls) != 1 || res.ExecCalls[0].Path != "/opt/app/qoder-cn" {
		t.Fatalf("AT_EMPTY_PATH 应使用 fd 注解，得到 %#v", res.ExecCalls)
	}
}

func TestParseExecCallFailed(t *testing.T) {
	res := parseExecTraceLine(t, `1234 execve("/opt/app/qoder-cn", ["qoder-cn"], 0x7ffd) = -1 ENOENT (No such file or directory)`)
	if len(res.ExecCalls) != 0 || res.ExecSeen {
		t.Fatalf("失败 exec 不应留下证据：calls=%#v seen=%v", res.ExecCalls, res.ExecSeen)
	}
}

func TestParseExecCallLoose(t *testing.T) {
	cases := []struct{ name, line string }{
		{"返回值未知", `1234 execve("/opt/app/qoder-cn", ["qoder-cn", "--x"], 0x7ffd) = ?`},
		{"unfinished", `1234 execve("/opt/app/qoder-cn", ["qoder-cn", "--x"], 0x7ffd <unfinished ...>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := parseExecTraceLine(t, c.line)
			if len(res.ExecCalls) != 1 {
				t.Fatalf("宽松解析应记录证据，得到 %#v", res.ExecCalls)
			}
			if res.ExecCalls[0].RetKnown {
				t.Fatalf("宽松解析必须标 RetKnown=false")
			}
			if len(res.ExecCalls[0].Args) != 2 || res.ExecCalls[0].Args[1] != "--x" {
				t.Fatalf("宽松解析 argv 错：%q", res.ExecCalls[0].Args)
			}
			if len(res.Reads) != 0 {
				t.Fatalf("宽松 exec 证据不能进入 Reads：%v", res.Reads)
			}
			if res.Stats.ParseErrors != 0 {
				t.Fatalf("宽松 exec 解析不应计 ParseErrors，得到 %d", res.Stats.ParseErrors)
			}
		})
	}
}

func TestParseExecResumedSkipped(t *testing.T) {
	res := parseExecTraceLine(t, `1234 <... execve resumed>) = 0`)
	if len(res.ExecCalls) != 0 || res.Stats.ParseErrors != 0 {
		t.Fatalf("resumed 行应跳过且不计错：calls=%#v errors=%d", res.ExecCalls, res.Stats.ParseErrors)
	}
}

func TestExecArgsSkippedDoesNotAffectReadTrace(t *testing.T) {
	res := parseExecTraceLine(t, `1234 execve("/opt/app/qoder-cn", NULL, 0x7ffd) = 0`)
	if res.Stats.ExecArgsSkipped != 1 {
		t.Fatalf("argv 缺失应计 ExecArgsSkipped=1，得到 %d", res.Stats.ExecArgsSkipped)
	}
	if len(res.ExecCalls) != 0 {
		t.Fatalf("argv 不可信时不能记录基线：%#v", res.ExecCalls)
	}
	if res.Stats.ParseErrors != 0 {
		t.Fatalf("ExecArgsSkipped 不应影响 read_trace，ParseErrors=%d", res.Stats.ParseErrors)
	}
}

func TestExecCallRing(t *testing.T) {
	var b strings.Builder
	b.WriteString(`1 execve("/opt/app/target", ["target"], 0x0) = 0` + "\n")
	for i := 0; i < execCallLimit+50; i++ {
		b.WriteString(`2 execve("/opt/app/child", ["child", "--type=renderer"], 0x0) = 0` + "\n")
	}
	res := ParseStraceAll(strings.NewReader(b.String()), "/home/u")
	if len(res.ExecCalls) != execCallLimit {
		t.Fatalf("环形上限应为 %d，得到 %d", execCallLimit, len(res.ExecCalls))
	}
	if res.ExecCalls[0].Path != "/opt/app/target" {
		t.Fatalf("首条基线必须保留，得到 %q", res.ExecCalls[0].Path)
	}
	if last := res.ExecCalls[len(res.ExecCalls)-1]; last.Path != "/opt/app/child" {
		t.Fatalf("最近证据必须保留，得到 %q", last.Path)
	}
}

func TestExecEvidenceOnlyDoesNotAddReads(t *testing.T) {
	res := StraceResult{execEvidenceOnly: true}
	parseStraceLineInto(`1234 execve("/home/u/.config/App/bin/app", ["app"], 0x0) = 0`,
		"/home/u", &res, map[string]bool{}, map[string]bool{})
	if len(res.Reads) != 0 {
		t.Fatalf("execEvidenceOnly 不能把 exec 目标写进 Reads：%v", res.Reads)
	}
	if !res.ExecSeen || len(res.ExecCalls) != 1 {
		t.Fatalf("execEvidenceOnly 仍要保留活性/证据：seen=%v calls=%#v", res.ExecSeen, res.ExecCalls)
	}
}
