package main

import (
	"strings"
	"testing"

	"github.com/mensylisir/appclone/core"
)

// libVersion 是"没有 pkg-config 时唯一的版本来源"，它只能靠文件名推断，
// 所以每条规则都要有断言钉住——推断错一位，doctor 就会把一个不够版本的
// 系统报成 ok（§6.3 的"假通过"）。
func TestLibVersionFromSoname(t *testing.T) {
	cases := []struct {
		path, soname string
		want         string
	}{
		{"/usr/lib/libgtk-4.so.1.1400.4", "libgtk-4.so.1", "4.14.4"},
		{"/usr/lib/libgtk-4.so.1.2400.1", "libgtk-4.so.1", "4.24.1"},
		// libadwaita 的 libtool 编码与 GTK 不同，按同一套规则解码会得出错误结论，
		// 因此只认平 soname 与未知两种结果——宁可不给版本，也不给错的版本。
		{"/usr/lib/libadwaita-1.so.0.5.0", "libadwaita-1.so.0", ""},
		// 平的 soname（多数发行版的 libadwaita）：没有版本段，必须认"不知道"，
		// 不许猜成 0.0 或 1.0。
		{"/usr/lib/libadwaita-1.so.0", "libadwaita-1.so.0", ""},
		// 编码不可能是 1~99（那是把 libtool 的 current 误当 minor）。
		{"/usr/lib/libgtk-4.so.1.50.0", "libgtk-4.so.1", ""},
	}
	for _, c := range cases {
		got, _ := libVersion(c.path, c.soname)
		if got != c.want {
			t.Fatalf("libVersion(%q)=%q want %q", c.path, got, c.want)
		}
	}
}

func TestSonameMajor(t *testing.T) {
	for soname, want := range map[string]string{
		"libgtk-4.so.1":       "4",
		"libadwaita-1.so.0":   "1",
		"libgobject-2.0.so.0": "2",
		"libsomething.so.1":   "",
	} {
		if got := sonameMajor(soname); got != want {
			t.Fatalf("sonameMajor(%q)=%q want %q", soname, got, want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.10.0", "1.5", false}, // 字典序会误判成"更低"，必须按数字段比
		{"1.4.2", "1.5", true},
		{"4.14.2", "4.14", false},
		{"4.6", "4.14", true},
		{"", "1.5", true},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Fatalf("versionLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// probeBackend 决定 doctor 报 ok/warn/fail：两样都没时装 warn 是假通过，
// 用户会以为"不装 fuse 也能用"，直到第一次探测直接报错。
func TestProbeBackend(t *testing.T) {
	cases := []struct {
		fuse, devFuse, strace bool
		want                  string
	}{
		{true, true, false, "ok"},
		{true, true, true, "ok"},
		{true, false, true, "warn"},
		{false, false, true, "warn"},
		{false, true, true, "warn"},
		{false, true, false, "fail"},
		{true, false, false, "fail"},
		{false, false, false, "fail"},
	}
	for _, c := range cases {
		got, detail := probeBackend(c.fuse, c.devFuse, c.strace)
		if got != c.want {
			t.Fatalf("probeBackend(fuse=%v,devFuse=%v,strace=%v)=%q want %q", c.fuse, c.devFuse, c.strace, got, c.want)
		}
		if got == "ok" && detail != "" {
			t.Fatalf("ok 的 detail 应由调用方填路径，得到 %q", detail)
		}
		if got != "ok" && detail == "" {
			t.Fatalf("warn/fail 必须带可执行建议")
		}
	}
}

// --strict 的必需项必须与"产品能不能探测新应用"对齐：fuse 检查只有
// 在 fuse 与 strace 都不可用时才是 fail，见 probeBackend。
func TestRequiredIncludesProbeBackendFailure(t *testing.T) {
	if !required("fuse") {
		t.Fatal("fuse 检查在两种后端都缺时是 fail，--strict 必须把它算作必需项")
	}
	if required("dbus") || required("gtk4") {
		t.Fatal("降级项不应影响 --strict")
	}
}

// 独立密钥环是必需项：档案要求时能力不足必须判 fail（否则 --strict 放过一个
// 必定起不来的分身——运行时对 keyring-only 是硬失败）；没要求时缺能力只是
// warn，应用不用系统密钥环就不受影响。
func TestKeyringCheckAndRequired(t *testing.T) {
	a, _ := cliEnv(t)
	find := func(checks []check) *check {
		for i := range checks {
			if checks[i].ID == "keyring" {
				return &checks[i]
			}
		}
		return nil
	}

	a.env.KeyringCapsOverride = &core.KeyringCaps{Available: false}
	checks := runChecks(a)
	kr := find(checks)
	if kr == nil || kr.Status != "warn" {
		t.Fatalf("缺能力且档案不要求时应为 warn，得到 %+v", kr)
	}
	if !required("keyring") {
		t.Fatal("keyring 必须列为必需项")
	}
	applyKeyringRequired(checks, true)
	if kr.Status != "fail" || !strings.Contains(kr.Detail, "isolate_keyring=true") {
		t.Fatalf("档案要求时能力不足必须 fail 并说明原因，得到 %+v", kr)
	}

	a.env.KeyringCapsOverride = &core.KeyringCaps{
		Available: true, Daemon: "/usr/bin/gnome-keyring-daemon", DbusSend: "/usr/bin/dbus-send",
	}
	checks = runChecks(a)
	kr = find(checks)
	if kr == nil || kr.Status != "ok" || !strings.Contains(kr.Detail, "gnome-keyring-daemon") {
		t.Fatalf("能力可用时应为 ok 并点名可执行文件，得到 %+v", kr)
	}
	applyKeyringRequired(checks, true)
	if kr.Status != "ok" {
		t.Fatalf("能力可用时不得被降级：%+v", kr)
	}
}
