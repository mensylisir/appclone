package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mensylisir/appclone/core"
)

// 版本超前的档案必须拒绝加载并提示，绝不能当"档案失效"重新探测后覆盖——
// 那会把新版独有的字段永久抹掉（§5.5：退出码 2）。这里用 --profile-only
// 证明错误发生在任何写盘/探测之前。
// --fork 必须真的写进 instance.toml（数据层来源一旦丢失，下次 run 就静默
// 退化成空分身）；与 --profile-only 同用是用法错误，不能静默忽略。
func TestAddForkFlagPersists(t *testing.T) {
	a, root := cliEnv(t)
	// 单独执行子命令不会走 root 的 PersistentPreRunE（那里才解析 CLI 路径）。
	a.env.CliPath = "/usr/bin/appclone"
	home := filepath.Join(root, "home")
	dataDir := filepath.Join(home, ".config", "Demo")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "state"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(home, "demo-bin")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	desktop := filepath.Join(root, "xdg", "data", "applications", "demo.desktop")
	if err := os.WriteFile(desktop,
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec="+execPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: execPath,
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "manual",
		Env: map[string]string{},
	}
	if err := a.env.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	cmd := newAddCmd(a)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"demo", "work", "--fork"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	in, err := a.env.GetInstance("demo", "work")
	if err != nil {
		t.Fatal(err)
	}
	if !in.Fork {
		t.Fatal("--fork 没有持久化到 instance.toml")
	}

	// --profile-only 不会建实例，--fork 无处落地，必须拒绝。
	cmd = newAddCmd(a)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"demo", "--profile-only", "--fork"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("--fork 与 --profile-only 同用必须报用法错误")
	}
}

func TestAddRefusesVersionAheadProfile(t *testing.T) {
	a, root := cliEnv(t)
	base := filepath.Join(root, "xdg", "appclone")
	if err := os.MkdirAll(filepath.Join(base, "profiles.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "profiles.d", "demo.toml")
	orig := "version = 2\nexec = \"/usr/bin/true\"\npaths = [\"~/.config/Demo\"]\nfuture_only = \"keep-me\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newAddCmd(a)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"demo", "--profile-only"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("版本超前的档案必须报错")
	}
	var ce *core.Error
	if !isCoreError(err, &ce) || ce.Key != "err.profile.version_ahead" {
		t.Fatalf("错误键应为 err.profile.version_ahead，得到 %v", err)
	}

	// 一个字节都不能被改写。
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != orig {
		t.Fatalf("拒绝加载时不得改写档案：\n%s", after)
	}
	if !strings.Contains(string(after), "future_only") {
		t.Fatalf("未来字段必须原样保留：\n%s", after)
	}
}

// add --json 的 keyring 契约：isolate_keyring 是档案字段（持久、脚本要读），
// keyring_evidence 是本次探测的 run-scoped 证据——复用档案时是空串，
// 表示"没有证据"，不是"没有 keyring"。GUI 确认页读的是同一对字段。
func TestAddJSONKeyringContract(t *testing.T) {
	a, root := cliEnv(t)
	a.env.CliPath = "/usr/bin/appclone"
	home := filepath.Join(root, "home")
	dataDir := filepath.Join(home, ".config", "Demo")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(home, "demo-bin")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	desktop := filepath.Join(root, "xdg", "data", "applications", "demo.desktop")
	if err := os.WriteFile(desktop,
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec="+execPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, isolate bool) map[string]any {
		t.Helper()
		p := core.Profile{
			App: "demo", Version: core.ProfileVersion, Exec: execPath,
			Paths: []string{"~/.config/Demo"}, ProbeMethod: "manual",
			Env: map[string]string{}, IsolateKeyring: isolate, KeyringSet: true,
		}
		if err := a.env.SaveProfile(p); err != nil {
			t.Fatal(err)
		}
		cmd := newAddCmd(a)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetArgs([]string{"demo", "--profile-only"})
		out, err := captureStdout(t, func() error { return cmd.Execute() })
		if err != nil {
			t.Fatalf("add --profile-only 失败：%v", err)
		}
		// 外形是 {ok, data}（与 list/doctor 同一约定），字段在 data 里。
		var resp struct {
			OK   bool           `json:"ok"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatalf("输出不是 JSON：%v\n%s", err, out)
		}
		if !resp.OK {
			t.Fatalf("命令失败：%s", out)
		}
		return resp.Data
	}

	on := run(t, true)
	if on["isolate_keyring"] != true {
		t.Fatalf("isolate_keyring 应为 true：%v", on)
	}
	if v, ok := on["keyring_evidence"]; !ok || v != "" {
		t.Fatalf("复用档案时 keyring_evidence 必须是空串（无证据）：%v", on)
	}
	// 证据扫描被上限截断时非零；复用档案没有探测，必须是 0 而不是缺字段
	// （脚本用 0 区分"证据完整但没有"与"证据被截断"）。
	if v, ok := on["keyring_trace_dropped"]; !ok || v != float64(0) {
		t.Fatalf("keyring_trace_dropped 应为 0：%v", on)
	}
	off := run(t, false)
	if off["isolate_keyring"] != false {
		t.Fatalf("显式关闭必须原样输出 false（不能缺键）：%v", off)
	}
}
