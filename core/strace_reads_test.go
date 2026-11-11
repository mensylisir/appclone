package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const readFixture = `1234  openat(AT_FDCWD, "/home/u/.config/App/creds", O_RDONLY) = 3
1234  openat(AT_FDCWD, "/home/u/.config/App/creds", O_WRONLY|O_CREAT, 0666) = 4
1234  openat(AT_FDCWD, "rel-no-ann", O_RDONLY) = 5
1234  openat(AT_FDCWD</home/u/.config/App>, "rel-with-ann", O_RDONLY) = 6
1234  openat2(AT_FDCWD, "/home/u/.config/App/x", {flags=O_RDONLY, resolve=0}, 24) = 7</home/u/.config/App/x>
1234  openat2(AT_FDCWD, "/home/u/.config/App/y", {resolve=0}, 24) = 8
1234  openat(AT_FDCWD, "/home/u/.config/App/numeric", 0) = 9
1234  openat(AT_FDCWD, "/home/u/.config/App/hex", 0x241) = 9
1234  openat(AT_FDCWD, "/home/u/.config/App/old (deleted)", O_RDONLY) = 10
1234  openat(AT_FDCWD, "rel-ann", O_RDONLY) = 12</home/u/.config/App/ann (deleted)>
1234  openat(AT_FDCWD, "/home/u/.config/App/fail", O_RDONLY) = -1 ENOENT (No such file)
1234  execve("/usr/bin/app", ["app"], 0x0) = 0
1234  execveat(AT_FDCWD, "", [], AT_EMPTY_PATH) = 0
1234  openat(AT_FDCWD, "/home/u/.config/App/q", O_RDONLY) = ?
1234  openat(AT_FDCWD, "/home/u/.config/App/unfinished", O_RDONLY <unfinished ...>
1234  openat(AT_FDCWD, "/home/u/.config/App/paren", O_RDONLY) = 11
`

func TestParseStraceReads(t *testing.T) {
	res := ParseStraceAll(strings.NewReader(readFixture), "/home/u")
	want := map[string]bool{
		".config/App/creds":        true,
		".config/App/rel-with-ann": true,
		".config/App/x":            true,
		".config/App/y":            true, // flags 不可见仍按读计入
		".config/App/old":          true, // (deleted) 剥离
		".config/App/ann":          true, // 返回值注解同样要剥离 (deleted)
		".config/App/paren":        true,
	}
	got := map[string]bool{}
	for _, p := range res.Reads {
		got[p] = true
	}
	for p := range want {
		if !got[p] {
			t.Fatalf("缺读路径 %q，实际 %v", p, res.Reads)
		}
	}
	for _, p := range []string{
		".config/App/numeric", // 纯数字 flags 不计读
		".config/App/hex",     // 十六进制 flags 同样不可解码，不能伪造读证据
		".config/App/fail",    // 失败
		".config/App/unfinished",
	} {
		if got[p] {
			t.Fatalf("不该出现读路径 %q", p)
		}
	}
	if !res.Stats.Openat2FlagsHidden {
		t.Fatal("openat2 flags 不可见必须置 Openat2FlagsHidden")
	}
	if !res.Stats.NoFDAnnotation {
		t.Fatal("相对路径缺 dirfd 注解必须置 NoFDAnnotation")
	}
	if res.Stats.ReadsDropped < 2 { // rel-no-ann + AT_EMPTY_PATH
		t.Fatalf("ReadsDropped 至少 2，得到 %d", res.Stats.ReadsDropped)
	}
	if res.Stats.ParseErrors < 4 { // openat2 不可见 + 数字 flags + 十六进制 flags + `= ?` + unfinished
		t.Fatalf("ParseErrors 至少 3，得到 %d", res.Stats.ParseErrors)
	}
}

func TestParseStraceWriteLinkTargets(t *testing.T) {
	fixture := `link("/home/u/.config/App/old", "/home/u/.config/App/new") = 0
symlink("/home/u/.config/App/target", "/home/u/.config/App/link") = 0
linkat(AT_FDCWD, "/home/u/.config/App/a", AT_FDCWD, "/home/u/.config/App/b", 0) = 0
symlinkat("/home/u/.config/App/t", AT_FDCWD, "/home/u/.config/App/s") = 0
`
	entries := ParseStrace(strings.NewReader(fixture), "/home/u")
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Path] = true
	}
	for _, p := range []string{".config/App/new", ".config/App/link", ".config/App/b", ".config/App/s"} {
		if !got[p] {
			t.Fatalf("link/symlink 应取新建链接路径 %q，实际 %v", p, got)
		}
	}
}

// 返回值注解是绝对路径时同样可能带 ` (deleted)`；不剥离会把写证据记到
// 一个不存在的路径上，实例里对应的文件反而查不到。
func TestParseStraceWriteReturnAnnotationDeleted(t *testing.T) {
	fixture := `1234  unlinkat(AT_FDCWD, "rel", 0) = 0</home/u/.config/App/gone (deleted)>
`
	res := ParseStraceAll(strings.NewReader(fixture), "/home/u")
	if len(res.Writes) != 1 || res.Writes[0].Path != ".config/App/gone" {
		t.Fatalf("写证据应剥离返回值注解的 (deleted)：%+v", res.Writes)
	}
}

func TestParseStraceSpacesAndChinese(t *testing.T) {
	fixture := `1234  openat(AT_FDCWD, "/home/u/.config/App/空 格/凭据.json", O_RDONLY) = 3
`
	res := ParseStraceAll(strings.NewReader(fixture), "/home/u")
	if len(res.Reads) != 1 || res.Reads[0] != ".config/App/空 格/凭据.json" {
		t.Fatalf("空格/中文路径解析失败：%v", res.Reads)
	}
}

func TestStraceCommandWithGolden(t *testing.T) {
	argv := StraceCommandWith(StraceSpec{
		LogPath:  "/tmp/reads.fifo",
		Syscalls: []string{"open", "openat"},
		Y:        true,
		V:        true,
		Tracee:   []string{"bwrap", "--", "/bin/true"},
	})
	want := "strace -f -qq -s 4096 -y -v -e trace=open,openat -o /tmp/reads.fifo -- bwrap -- /bin/true"
	if got := strings.Join(argv, " "); got != want {
		t.Fatalf("命令形状错：\n got %s\nwant %s", got, want)
	}
}

// creat 没有 flags 参数，等价 O_WRONLY|O_CREAT|O_TRUNC：必须无条件算写，
// 否则"应用只建了一个文件"的探测会被误判为零写入。
func TestParseStraceCreatIsWrite(t *testing.T) {
	fixture := `1234  creat("/home/u/.config/App/new", 0666) = 3
`
	res := ParseStraceAll(strings.NewReader(fixture), "/home/u")
	if len(res.Writes) != 1 || res.Writes[0].Path != ".config/App/new" {
		t.Fatalf("creat 必须算写：%v", res.Writes)
	}
	if len(res.Reads) != 0 {
		t.Fatalf("creat 不应产生读证据：%v", res.Reads)
	}
}

// 相对路径没有参数侧 dirfd 注解时，用返回值注解（-y 的 fd 真实路径）兜底。
func TestParseStraceReadFallsBackToReturnAnnotation(t *testing.T) {
	fixture := `1234  openat(AT_FDCWD, "rel.txt", O_RDONLY) = 3</home/u/.config/App/rel.txt>
`
	res := ParseStraceAll(strings.NewReader(fixture), "/home/u")
	if len(res.Reads) != 1 || res.Reads[0] != ".config/App/rel.txt" {
		t.Fatalf("返回值注解兜底失败：%v", res.Reads)
	}
}

// homeRel 必须在词法命中后验证真实落点：`~/.config/App` 软链到 HOME 外时，
// 里面读到的文件不属于家目录，不能被当成隔离候选。
func TestHomeRelRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".config", "App")); err != nil {
		t.Skipf("symlink 不可用：%v", err)
	}
	if rel, ok := homeRel(home, filepath.Join(home, ".config", "App", "f")); ok {
		t.Fatalf("软链逃出 HOME 的路径不能算家目录内：%q", rel)
	}
	// 软链仍指向 HOME 内时必须接受。
	if err := os.MkdirAll(filepath.Join(home, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "real"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "real", "g"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if rel, ok := homeRel(home, filepath.Join(home, "link", "g")); !ok || rel != "link/g" {
		t.Fatalf("HOME 内软链应保留词法路径：%q %v", rel, ok)
	}
}

// execveat 的相对路径要用参数侧 dirfd 注解归属；AT_EMPTY_PATH 只看 flags
// 参数，不能被路径文本里的同名子串误判。
func TestParseStraceExecveatPaths(t *testing.T) {
	fixture := `1 execveat(3</home/u/.config/App>, "./app", ["app"], 0x0, 0) = 0
2 execveat(AT_FDCWD, "/home/u/AT_EMPTY_PATH/app", ["app"], 0x0, 0) = 0
3 execveat(AT_FDCWD, "", [""], 0x0, AT_EMPTY_PATH) = 0
`
	res := ParseStraceAll(strings.NewReader(fixture), "/home/u")
	got := map[string]bool{}
	for _, p := range res.Reads {
		got[p] = true
	}
	if !got[".config/App/app"] {
		t.Fatalf("相对 execveat 应经 dirfd 归属：%v", res.Reads)
	}
	if !got["AT_EMPTY_PATH/app"] {
		t.Fatalf("路径里的 AT_EMPTY_PATH 不能被误判：%v", res.Reads)
	}
	if res.Stats.ReadsDropped != 1 { // 第 3 行：AT_FDCWD 空路径无法归属
		t.Fatalf("ReadsDropped 应为 1，得到 %d", res.Stats.ReadsDropped)
	}
	if len(res.Execs) < 2 {
		t.Fatalf("解析后的 exec 目标应记录：%v", res.Execs)
	}
}
