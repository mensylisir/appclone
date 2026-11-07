package main

import "testing"

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
