package main

import (
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
