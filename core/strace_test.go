package core

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const straceFixture = `1234  openat(AT_FDCWD, "/home/u/.config/App/config.ini", O_WRONLY|O_CREAT|O_CLOEXEC, 0666) = 3
1234  openat(AT_FDCWD, "/home/u/.config/App/read-only.ini", O_RDONLY|O_CLOEXEC) = 4
1234  open("/home/u/.config/App/O_PATH", O_PATH|O_CLOEXEC) = 5
1234  mkdir("/home/u/.local/share/App", 0700) = 0
1234  mkdir("/home/u/.local/share/App", 0700) = -1 EEXIST (File exists)
1234  unlink("/home/u/.config/App/old.lock") = 0
1234  rename("/home/u/.config/App/tmp", "/home/u/.config/App/final") = 0
1234  openat(AT_FDCWD, "/home/u/Downloads/saved.bin", O_WRONLY|O_CREAT, 0666) = 6
1234  openat(AT_FDCWD, "/etc/shadow", O_WRONLY) = -1 EACCES (Permission denied)
1234  openat(AT_FDCWD, "relative/path", O_WRONLY) = 7
1234  openat(AT_FDCWD, "/home/u/.config/App/x", O_WRONLY <unfinished ...>
1234  <... openat resumed>) = 8
1234  stat("/home/u/.config/App/config.ini", {st_mode=S_IFREG}) = 0
`

// strace 只能看到"打开过什么"，所以过滤规则必须是"有写入意图"：
// 只读打开、O_PATH、stat 全部跳过；否则每个应用都会把整个家目录的
// 读取路径贡献成候选。
func TestParseStraceFiltersReads(t *testing.T) {
	entries := ParseStrace(strings.NewReader(straceFixture), "/home/u")
	got := map[string]EntryKind{}
	for _, e := range entries {
		got[e.Path] = e.Kind
	}
	want := map[string]EntryKind{
		".config/App/config.ini": EntryFile,
		".local/share/App":       EntryDir,
		".config/App/old.lock":   EntryWhiteout, // unlink → 删除证据，不是写入
		".config/App/final":      EntryFile,     // rename 只记目标
		"Downloads/saved.bin":    EntryFile,     // 公共目录由分类阶段排除，解析层不丢
	}
	for path, kind := range want {
		k, ok := got[path]
		if !ok {
			t.Fatalf("缺少条目 %q，实际：%v", path, got)
		}
		if k != kind {
			t.Fatalf("%q 的类型应为 %v，得到 %v", path, kind, k)
		}
	}
	for _, skip := range []string{
		".config/App/read-only.ini", // O_RDONLY
		".config/App/O_PATH",        // O_PATH
		".config/App/x",             // unfinished/resumed 被跳过
	} {
		if _, ok := got[skip]; ok {
			t.Fatalf("不该出现 %q：只读或拼接行不能当写入", skip)
		}
	}
	// HOME 之外与相对路径一律丢弃。
	for p := range got {
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "..") {
			t.Fatalf("不该保留绝对路径或逃出 HOME 的条目：%q", p)
		}
	}
}

// 删除类调用与写入必须分开：混在一起会让用户在已删路径上勾"隔离"。
func TestParseStraceSeparatesWhiteouts(t *testing.T) {
	entries := ParseStrace(strings.NewReader(straceFixture), "/home/u")
	var writes, whiteouts int
	for _, e := range entries {
		if e.Kind == EntryWhiteout {
			whiteouts++
		} else {
			writes++
		}
	}
	if whiteouts != 1 || writes != 4 {
		t.Fatalf("应 1 条 whiteout / 4 条写入，得到 %d / %d", whiteouts, writes)
	}
}

// strace 必须跑在沙箱**外面**：日志由它在宿主写，不用操心"沙箱里往哪写"。
func TestStraceCommandShape(t *testing.T) {
	inner := []string{"bwrap", "--bind", "/", "/", "--", "/usr/bin/app"}
	argv := StraceCommand("/var/tmp/p/round-1.log", inner)
	joined := strings.Join(argv, " ")
	if !strings.HasPrefix(joined, "strace -f ") {
		t.Fatalf("应以 strace -f 开头：%s", joined)
	}
	if !strings.Contains(joined, "-o /var/tmp/p/round-1.log") {
		t.Fatalf("必须指定日志文件：%s", joined)
	}
	if !strings.Contains(joined, "trace=%file") {
		t.Fatalf("应只跟踪文件类系统调用：%s", joined)
	}
	if !strings.HasSuffix(joined, "-- bwrap --bind / / -- /usr/bin/app") {
		t.Fatalf("内层命令必须原样跟在 -- 之后：%s", joined)
	}
	// 内层不带 overlay：HOME 就是真实的，也就没有"把公共目录绑回去"这回事。
	innerArgs, err := ProbeBwrapArgs(ProbeSpec{
		Home: "/home/u", Command: []string{"/usr/bin/app"}, NoOverlay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(innerArgs, " "), "--bind  /home/u") {
		t.Fatalf("NoOverlay 时不该有 merged→HOME 的 bind：%v", innerArgs)
	}
}

// logRunner 模拟"应用跑完了、strace 写好日志"：从 argv 里找出 -o 的目标写进去。
type logRunner struct {
	body  string
	calls int
}

func (l *logRunner) Run(_ context.Context, argv []string, _ io.Writer) (SessionResult, error) {
	l.calls++
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "-o" {
			if err := os.WriteFile(argv[i+1], []byte(l.body), 0o600); err != nil {
				return SessionResult{}, err
			}
		}
	}
	return SessionResult{}, nil
}

// 降级模式的完整链路：strace → 解析 → 归并 → 档案（probe_method = strace）。
func TestProbeStraceBuildsProfile(t *testing.T) {
	e := probeEnv(t)
	home := e.Resolver.Home
	body := "1  openat(AT_FDCWD, \"" + filepath.Join(home, ".config", "App", "config.ini") + "\", O_WRONLY|O_CREAT, 0666) = 3\n" +
		"1  openat(AT_FDCWD, \"" + filepath.Join(home, ".cache", "App", "blob") + "\", O_WRONLY|O_CREAT, 0666) = 4\n"
	r := &logRunner{body: body}
	res, err := e.probeStrace(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/nonexistent/zz-probe-test-app"},
		Runner:  r,
		Rounds:  1,
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile.ProbeMethod != "strace" {
		t.Fatalf("降级路径必须记 probe_method=strace，得到 %q", res.Profile.ProbeMethod)
	}
	if res.Profile.Exec != "/nonexistent/zz-probe-test-app" {
		t.Fatalf("档案必须自带 exec：%q", res.Profile.Exec)
	}
	paths := strings.Join(res.Profile.Paths, ",")
	if !strings.Contains(paths, "~/.config/App") {
		t.Fatalf("写入的配置目录应进 paths：%v", res.Profile.Paths)
	}
	if strings.Contains(paths, ".cache") {
		t.Fatalf("缓存不该进 paths：%v", res.Profile.Paths)
	}
	if r.calls != 1 {
		t.Fatalf("Rounds=1 时只该跑一轮，实际 %d", r.calls)
	}
}

// 降级不是实现细节：界面要靠这条事件起常驻横幅，否则用户拿到的是一份
// 低置信度档案却看不出来（tech-stack §4.5 探测页）。
func TestProbeReportsFallbackToGUI(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("没有 strace，降级路径不可达")
	}
	e := probeEnv(t)
	r := &logRunner{body: ""}
	var stages []string
	_, err := e.Probe(context.Background(), "testapp", ProbeOptions{
		Command:  []string{"/nonexistent/zz-probe-test-app"},
		Mounter:  &fakeMounter{failOn: "available"},
		Runner:   r,
		Rounds:   1,
		Progress: func(p ProbeProgress) { stages = append(stages, p.Stage) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(stages, "fallback") {
		t.Fatalf("降级必须上报 fallback 事件，实际事件：%v", stages)
	}
}
