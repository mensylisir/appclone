//go:build linux

package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSelfRelaunchCaptureE2E 用真实 bwrap + strace 验证“namespace teardown
// 之前能稳定捕获应用自重启的 execve”。它不依赖 qoder，只用脚本模拟：
// 首次启动带 shebang 执行，然后后台自重启并追加 --ozone-platform=x11。
//
// 20/20 是硬门槛：如果 execve 证据在拆毁前不可靠，自动预填就不该上线。
// 环境缺失（无 bwrap/strace、无法建 namespace）时 skip，不把环境问题伪装成
// 通过或失败。
func TestSelfRelaunchCaptureE2E(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap 不可用")
	}
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace 不可用")
	}
	// Home 与 TraceDir 必须是两个目录：TraceDir 会在 bwrap 里被 tmpfs 遮蔽。
	home := t.TempDir()
	traceDir := t.TempDir()
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "fake-qoder")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--ozone-platform=x11\" ]; then sleep 0.1; exit 0; fi\n" +
		"( \"$0\" --ozone-platform=x11 >/dev/null 2>&1 & )\n" +
		"sleep 0.2\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	const iterations = 10
	ok := 0
	for iter := 0; iter < iterations; iter++ {
		var all []ExecCall
		for round := 0; round < 2; round++ {
			tracePath := filepath.Join(traceDir, fmt.Sprintf("iter-%d-round-%d.trace", iter, round))
			inner, err := ProbeBwrapArgs(ProbeSpec{
				Home: home, NoOverlay: true, TraceDir: traceDir,
				Command: []string{script},
			})
			if err != nil {
				t.Fatalf("构造 bwrap 参数失败：%v", err)
			}
			argv := StraceCommandWith(StraceSpec{
				LogPath: tracePath, Syscalls: []string{"execve", "execveat"},
				V: true, Tracee: inner,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			out, runErr := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
			cancel()
			if runErr != nil {
				t.Skipf("bwrap/strace 环境不可用（iter %d round %d）：%v\n%s", iter, round, runErr, out)
			}
			f, err := os.Open(tracePath)
			if err != nil {
				t.Fatalf("打开 trace 失败：%v", err)
			}
			res := ParseStraceAll(f, home)
			_ = f.Close()
			for i := range res.ExecCalls {
				res.ExecCalls[i].Round = round
			}
			all = append(all, res.ExecCalls...)
		}

		// 首个同路径证据必须是脚本本身：没有 shebang 时 execvp 会回退到
		// /bin/sh，detector 永远匹配不到目标。
		first := ""
		for _, c := range all {
			if sameExecPath(c.Path, script) {
				first = c.Path
				break
			}
		}
		if first != script {
			t.Fatalf("iter %d：首个同路径 exec 不是脚本（%q）；trace 里的路径可能被 execvp 回退改写", iter, first)
		}

		sr := DetectSelfRelaunchArgs(script, nil, all, 2)
		if !sr.OK || len(sr.Delta) != 1 || sr.Delta[0] != "--ozone-platform=x11" {
			t.Fatalf("iter %d：未识别出自重启（%#v）", iter, sr)
		}
		ok++
	}
	if ok != iterations {
		t.Fatalf("捕获率 %d/%d，要求 %d/%d", ok, iterations, iterations, iterations)
	}
}
