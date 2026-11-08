package i18n

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// placeholder 匹配消息模板里的 {name} 占位符。字母大小写与数字都算：
// 将来写成 {N} 或 {path2} 也不能静默漏检。
var placeholder = regexp.MustCompile(`\{[A-Za-z0-9_]+\}`)

// cjk 用来抓 en 表里混入的中文。
var cjk = regexp.MustCompile(`[\p{Han}]`)

// TestTablesMatch：两种语言的键必须一一对应，占位符集合也必须一致。
//
// GUI 文案已按 ADR-027 全部进表：漏一条 en 译文时 T() 会静默回落中文，
// 英文界面上出现一段中文；漏一个占位符则渲染后直接留下字面量 {n}。
// 这两种缺陷都不会报错，只能在这里拦。
func TestTablesMatch(t *testing.T) {
	zh, en := messages["zh_CN"], messages["en"]
	if len(zh) == 0 || len(en) == 0 {
		t.Fatal("消息表为空")
	}
	for key, zhText := range zh {
		enText, ok := en[key]
		if !ok {
			t.Errorf("键 %s 只有 zh_CN，没有 en", key)
			continue
		}
		zhArgs := argsOf(zhText)
		enArgs := argsOf(enText)
		if len(zhArgs) != len(enArgs) {
			t.Errorf("键 %s 的占位符不一致：zh=%v en=%v", key, zhArgs, enArgs)
			continue
		}
		for a := range zhArgs {
			if !enArgs[a] {
				t.Errorf("键 %s 的占位符 {%s} 只在 zh_CN 里出现", key, a)
			}
		}
	}
	for key := range en {
		if _, ok := zh[key]; !ok {
			t.Errorf("键 %s 只有 en，没有 zh_CN", key)
		}
	}
	// en 表里混入中文 = 翻译漏了，会直接显示在英文界面上。
	for key, text := range en {
		if cjk.MatchString(text) {
			t.Errorf("键 %s 的 en 文案里还有中文：%q", key, text)
		}
	}
}

// TestCoreKeysExist：core 源码里出现的每个字面量 err.* / probe.why.* 键都必须在表里。
//
// 只比两张表发现不了"core 新加了一个 key、表和 GUI 都没跟上"——那时用户
// 看到的是原始键名（err.internal.probe_spec）。扫描生产源码，宁可误报
// 一条注释里的键，也不要让缺键漏到界面上。
func TestCoreKeysExist(t *testing.T) {
	files, err := filepath.Glob("../*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("找不到 core 源码")
	}
	keyRE := regexp.MustCompile(`"(?:err|probe\.why)\.[A-Za-z0-9_.]+"`)
	seen := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range keyRE.FindAllString(string(data), -1) {
			key := m[1 : len(m)-1]
			seen[key] = filepath.Base(f)
		}
	}
	if len(seen) == 0 {
		t.Fatal("没有从 core 源码里提取到任何 err.* 键（扫描规则失效？）")
	}
	for key, file := range seen {
		if _, ok := messages["zh_CN"][key]; !ok {
			t.Errorf("core/%s 用了键 %s，但 zh_CN 表里没有", file, key)
		}
		if _, ok := messages["en"][key]; !ok {
			t.Errorf("core/%s 用了键 %s，但 en 表里没有", file, key)
		}
	}
}

// TestViewLiteralKeysExist：两个视图（GUI + CLI）里每个字面量 ui.* 键
// （以及它们直接引用的 err.* / probe.why.*）都必须在表里。改名一个键而
// 漏改表，界面/终端会直接显示裸键名——这类缺陷不会报错，只能靠扫描源码拦。
func TestViewLiteralKeysExist(t *testing.T) {
	var files []string
	for _, dir := range []string{"../../cmd/appclone-gui", "../../cmd/appclone"} {
		found, err := filepath.Glob(dir + "/*.go")
		if err != nil {
			t.Fatal(err)
		}
		if len(found) == 0 {
			t.Fatalf("找不到 %s 源码", dir)
		}
		files = append(files, found...)
	}
	keyRE := regexp.MustCompile(`"(?:ui|err|probe\.why)\.[A-Za-z0-9_.]+"`)
	seen := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range keyRE.FindAllString(string(data), -1) {
			seen[m[1:len(m)-1]] = filepath.Base(f)
		}
	}
	if len(seen) < 100 {
		t.Fatalf("只从视图源码里提取到 %d 个键（扫描规则失效？）", len(seen))
	}
	for key, file := range seen {
		if _, ok := messages["zh_CN"][key]; !ok {
			t.Errorf("视图 %s 用了键 %s，但 zh_CN 表里没有", file, key)
		}
		if _, ok := messages["en"][key]; !ok {
			t.Errorf("视图 %s 用了键 %s，但 en 表里没有", file, key)
		}
	}
}

// TestTReplaceAndFallback 钉住两条容易改坏的语义：未知语言回落英文、
// 缺键返回键本身（而不是静默给一句别的语言）。
func TestTReplaceAndFallback(t *testing.T) {
	if got := T("zh_CN", "ui.app.subtitle.running", map[string]any{"n": 2, "running": 1}); got != "2 个分身 · 1 个运行中" {
		t.Fatalf("zh_CN 渲染结果不对：%q", got)
	}
	if got := T("xx_YY", "ui.empty.pick", nil); got != "Choose an app" {
		t.Fatalf("未知语言应回落英文，得到 %q", got)
	}
	if got := T("zh_CN", "ui.no.such.key", nil); got != "ui.no.such.key" {
		t.Fatalf("两张表都没有的键应返回键本身，得到 %q", got)
	}
	// 切片参数的分隔符必须跟语言走，否则英文界面会出现「、」。
	if got := T("en", "err.instance.exists", map[string]any{"app": "demo", "id": "main", "existing": []string{"a", "b"}}); !strings.Contains(got, "a, b") {
		t.Fatalf("en 切片参数没有用英文分隔符：%q", got)
	}
	if got := T("zh_CN", "err.instance.exists", map[string]any{"app": "demo", "id": "main", "existing": []string{"a", "b"}}); !strings.Contains(got, "a、b") {
		t.Fatalf("zh 切片参数没有用顿号：%q", got)
	}
	// 渲染必须只扫模板一遍：参数值里出现的 {n}/$1 是用户数据（错误详情），
	// 不能被当成占位符再替换一次。换成"按参数名逐个 ReplaceAll"就会回归。
	got := T("en", "ui.toast.delete_failed", map[string]any{"n": 5, "detail": "x {n} $1 ${y}"})
	if !strings.Contains(got, "x {n} $1 ${y}") {
		t.Fatalf("参数值里的占位符被二次替换了：%q", got)
	}
	if !strings.Contains(got, "5") {
		t.Fatalf("模板占位符没有被替换：%q", got)
	}
	// strace 超时没有现场可保留，文案不许带 {quarantine} 占位符（否则
	// 调用点没参数可传，界面会直接显示字面量）。
	for _, lang := range []string{"zh_CN", "en"} {
		if msg := T(lang, "err.probe.timeout_strace", nil); strings.Contains(msg, "{") {
			t.Fatalf("%s 的 err.probe.timeout_strace 不应含占位符：%q", lang, msg)
		}
	}
}

// TestDetectLang 覆盖 ADR-025 的语言判定顺序与 v1 的简体边界。
func TestDetectLang(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "en"},
		{map[string]string{"LANG": "zh_CN.UTF-8"}, "zh_CN"},
		{map[string]string{"LANG": "zh-CN"}, "zh_CN"},
		{map[string]string{"LANG": "zh_SG.UTF-8"}, "zh_CN"},
		{map[string]string{"LANG": "zh_Hans_CN.UTF-8"}, "zh_CN"},
		// 繁体不做（v1）：不能因为 zh 前缀就给简体。
		{map[string]string{"LANG": "zh_TW.UTF-8"}, "en"},
		{map[string]string{"LANG": "zh_HK.UTF-8"}, "en"},
		{map[string]string{"LANG": "en_US.UTF-8"}, "en"},
		{map[string]string{"LANG": "de_DE.UTF-8"}, "en"},
		// LC_ALL > LC_MESSAGES > LANG
		{map[string]string{"LC_ALL": "en_US.UTF-8", "LANG": "zh_CN.UTF-8"}, "en"},
		{map[string]string{"LC_MESSAGES": "zh_CN.UTF-8", "LANG": "en_US.UTF-8"}, "zh_CN"},
		{map[string]string{"LANG": "zh_CN.UTF-8@modifier"}, "zh_CN"},
	}
	for _, c := range cases {
		getenv := func(k string) string { return c.env[k] }
		if got := DetectLang(getenv); got != c.want {
			t.Errorf("DetectLang(%v) = %q，期望 %q", c.env, got, c.want)
		}
	}
}

func argsOf(tmpl string) map[string]bool {
	out := map[string]bool{}
	for _, m := range placeholder.FindAllString(tmpl, -1) {
		out[m[1:len(m)-1]] = true
	}
	return out
}
