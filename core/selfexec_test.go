package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func execCall(path string, round int, known bool, argv ...string) ExecCall {
	return ExecCall{Path: path, Args: argv, Round: round, RetKnown: known}
}

// manyFlags 生成 n 个互不相同、无危险语义的长旗标，用于验证增量上限。
func manyFlags(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("--feature-%d", i)
	}
	return out
}

func TestSameExecPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "qoder")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "qoder-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameExecPath(real, real) {
		t.Fatal("同路径应相等")
	}
	if !sameExecPath(link, real) {
		t.Fatal("软链与真实路径应视为同一文件")
	}
	if sameExecPath("", real) || sameExecPath(real, "") {
		t.Fatal("空字符串必须 false")
	}
	if sameExecPath(filepath.Join(dir, "a"), filepath.Join(dir, "b")) {
		t.Fatal("不同路径不应相等")
	}
}

func TestDetectSelfRelaunchArgs(t *testing.T) {
	const q = "/opt/Qoder CN/qoder-cn"
	ozone := "--ozone-platform=x11"
	cases := []struct {
		name      string
		initial   []string
		calls     []ExecCall
		min       int
		wantOK    bool
		wantArgs  []string
		wantKnown bool
		wantAmb   bool
	}{
		{
			name: "空基线 + 两轮 ozone",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, ozone),
				execCall(q, 1, true, q),
				execCall(q, 1, true, q, ozone),
			},
			min:       2,
			wantOK:    true,
			wantArgs:  []string{ozone},
			wantKnown: true,
		},
		{
			name:    "initial 已有 --no-sandbox",
			initial: []string{"--no-sandbox"},
			calls: []ExecCall{
				execCall(q, 0, true, q, "--no-sandbox"),
				execCall(q, 0, true, q, "--no-sandbox", ozone),
				execCall(q, 1, true, q, "--no-sandbox", ozone),
			},
			min:       2,
			wantOK:    true,
			wantArgs:  []string{"--no-sandbox", ozone},
			wantKnown: true,
		},
		{
			name: "只有基线没有扩展",
			calls: []ExecCall{
				execCall(q, 0, true, q, "--no-sandbox"),
				execCall(q, 1, true, q, "--no-sandbox"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "子进程选择器忽略",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--type=zygote"),
				execCall(q, 1, true, q, "--type=zygote"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "基线自身带 --type 仍保留",
			calls: []ExecCall{
				execCall(q, 0, true, q, "--type=renderer"),
				execCall(q, 0, true, q, "--type=renderer", ozone),
				execCall(q, 1, true, q, "--type=renderer", ozone),
			},
			min:       2,
			wantOK:    true,
			wantArgs:  []string{ozone},
			wantKnown: true,
		},
		{
			name: "危险旗标拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--user-data-dir=/tmp/x"),
				execCall(q, 1, true, q, "--user-data-dir=/tmp/x"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "凭据旗标拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--token=abc"),
				execCall(q, 1, true, q, "--token=abc"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "多种 delta 歧义",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--a"),
				execCall(q, 0, true, q, "--b"),
				execCall(q, 1, true, q, "--a"),
				execCall(q, 1, true, q, "--b"),
			},
			min:     2,
			wantOK:  false,
			wantAmb: true,
		},
		{
			name: "同一轮两次且只有一轮",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, ozone),
				execCall(q, 0, true, q, ozone),
			},
			min:     2,
			wantOK:  false,
			wantAmb: true,
		},
		{
			name: "同一轮两次 + 另一轮一次",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, ozone),
				execCall(q, 0, true, q, ozone),
				execCall(q, 1, true, q, ozone),
			},
			min:       2,
			wantOK:    true,
			wantArgs:  []string{ozone},
			wantKnown: true,
		},
		{
			name: "仅 Round 0 一次",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, ozone),
			},
			min:     2,
			wantOK:  false,
			wantAmb: true,
		},
		{
			name:    "initial 已含 delta 不重复",
			initial: []string{ozone},
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, ozone),
				execCall(q, 1, true, q, ozone),
			},
			min:    2,
			wantOK: false, // filteredDelta 为空
		},
		{
			name: "known 存在时忽略不同 unknown",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, ozone),
				execCall(q, 1, false, q, "--other"),
			},
			min:       1,
			wantOK:    true,
			wantArgs:  []string{ozone},
			wantKnown: true,
		},
		{
			name: "同 delta known+unknown 合并轮次",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, ozone),
				execCall(q, 1, false, q, ozone),
			},
			min:       2,
			wantOK:    true,
			wantArgs:  []string{ozone},
			wantKnown: true,
		},
		{
			name: "只有 unknown delta 两轮接受",
			calls: []ExecCall{
				execCall(q, 0, false, q),
				execCall(q, 0, false, q, ozone),
				execCall(q, 1, false, q, ozone),
			},
			min:       2,
			wantOK:    true,
			wantArgs:  []string{ozone},
			wantKnown: false,
		},
		{
			name: "裸 - 不是旗标",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "-"),
				execCall(q, 1, true, q, "-"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "危险边界旗标变体拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--state-dir=/tmp/s"),
				execCall(q, 1, true, q, "--state-dir=/tmp/s"),
			},
			min:    2,
			wantOK: false,
		},
		{
			// 危险词前加前缀不能绕过（Contains 匹配）。
			name: "危险旗标前缀变体拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--disable-sandbox"),
				execCall(q, 1, true, q, "--disable-sandbox"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "危险旗标后缀变体拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--app-config=/tmp/c"),
				execCall(q, 1, true, q, "--app-config=/tmp/c"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "危险旗标加 x- 前缀拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--x-user-data-dir=/tmp/u"),
				execCall(q, 1, true, q, "--x-user-data-dir=/tmp/u"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "危险旗标下划线写法拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "--app_profile=work"),
				execCall(q, 1, true, q, "--app_profile=work"),
			},
			min:    2,
			wantOK: false,
		},
		{
			// 只接受长旗标：短旗标/单词参数语义不明，预填可能改变应用行为。
			name: "短旗标拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "-v"),
				execCall(q, 1, true, q, "-v"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "位置参数拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, q, "work"),
				execCall(q, 1, true, q, "work"),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "增量超过上限拒绝",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, append([]string{q}, manyFlags(17)...)...),
				execCall(q, 1, true, append([]string{q}, manyFlags(17)...)...),
			},
			min:    2,
			wantOK: false,
		},
		{
			name: "增量恰为上限接受",
			calls: []ExecCall{
				execCall(q, 0, true, q),
				execCall(q, 0, true, append([]string{q}, manyFlags(16)...)...),
				execCall(q, 1, true, append([]string{q}, manyFlags(16)...)...),
			},
			min:       2,
			wantOK:    true,
			wantArgs:  manyFlags(16),
			wantKnown: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DetectSelfRelaunchArgs(q, c.initial, c.calls, c.min)
			if got.OK != c.wantOK {
				t.Fatalf("OK=%v，期望 %v（结果 %#v）", got.OK, c.wantOK, got)
			}
			if got.Ambiguous != c.wantAmb {
				t.Fatalf("Ambiguous=%v，期望 %v（结果 %#v）", got.Ambiguous, c.wantAmb, got)
			}
			if c.wantOK {
				if got.Known != c.wantKnown {
					t.Fatalf("Known=%v，期望 %v", got.Known, c.wantKnown)
				}
				if len(got.Args) != len(c.wantArgs) {
					t.Fatalf("Args=%q，期望 %q", got.Args, c.wantArgs)
				}
				for i := range got.Args {
					if got.Args[i] != c.wantArgs[i] {
						t.Fatalf("Args=%q，期望 %q", got.Args, c.wantArgs)
					}
				}
			}
		})
	}
}

// TestDangerousSelfRelaunchFlag 固定“自动预填不得改写数据目录/沙箱/调试/
// 代理/日志边界或携带凭据”的拒绝面（ADR-036 威胁模型）。新增危险旗标时
// 先在这里补一行，再改 denylist。
func TestDangerousSelfRelaunchFlag(t *testing.T) {
	denied := []string{
		"--user-data-dir=/tmp/x", "--profile-directory=p", "--profile=p",
		"--config=/tmp/c", "--config-dir=/tmp/c", "--home=/tmp/h",
		"--state-dir=/tmp/s", "--runtime-dir=/tmp/r", "--tmp-dir=/tmp/t",
		"--work-dir=/tmp/w", "--cwd=/tmp", "--path=/tmp/p", "--root=/tmp",
		"--prefix=/tmp/p", "--log-file=/tmp/l", "--enable-logging",
		"--proxy-server=http://x", "--proxy=http://x", "--debug-port=1",
		"--remote-debugging-port=1", "--remote-debugging-pipe",
		"--inspect=9229", "--js-flags=--x", "--no-sandbox", "--sandbox",
		"--disable-setuid-sandbox", "--disable-seccomp-filter-sandbox",
		"--disable-gpu-sandbox", "--single-process", "--disable-web-security",
		"--ignore-certificate-errors", "--ssl-key-log-file=/tmp/k",
		"--token=abc", "--access-token=abc", "--session-token=abc",
		"--client-secret=abc", "--api-key=abc", "--private-key=/tmp/k",
		"--keystore=/tmp/k", "--password=abc", "--cookie=abc",
		"--dbus-address=x", "--socket=/tmp/s", "--host-resolver-rules=x",
		"--rootdir=/tmp/r", "--apppath=/tmp/a", "--keypath=/tmp/k", "--homepath=/tmp/h",
		// 粘连/驼峰形式：短凭据词与 log/root/dbus/sandbox 的复合写法
		"--x-user-data-dir=/tmp/u", "--dbpassword=abc", "--userpass=abc",
		"--clientcert=/tmp/c", "--basicauth=abc", "--logdir=/tmp/l",
		"--logDirectory=/tmp/l", "--logpath=/tmp/l", "--unsandboxed",
		"--rootfs=/tmp/r", "--dbuspath=/tmp/d",
		// 对抗性大小写：全名归一化后仍须命中长词
		"--dIsAbLeWeBsEcUrItY", "--hOsTrEsOlVeRrUlEs",
		"-", "--",
	}
	for _, arg := range denied {
		if !dangerousSelfRelaunchFlag(arg) {
			t.Errorf("%q 应被拒绝", arg)
		}
	}
	allowed := []string{
		"--ozone-platform=x11", "--ozone-platform-hint=auto",
		"--enable-wayland-ime", "--wayland-text-input-version=3",
		"--force-device-scale-factor=1.5", "--disable-gpu",
		"--disable-dev-shm-usage", "-v", "-abc",
		// 危险词只是更长单词的一部分，不能误拒（旧 Contains 会拒掉这些）：
		"--catalog=/tmp/c", "--bypass", "--compass", "--author=me",
		"--rootless", "--attempts=3", "--hotdog",
	}
	for _, arg := range allowed {
		if dangerousSelfRelaunchFlag(arg) {
			t.Errorf("%q 不应被拒绝", arg)
		}
	}
}
