package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mensylisir/appclone/core"
)

// stop 对没在跑的分身必须退出 7（状态不允许），而不是静默成功——
// 脚本需要能区分"停了"和"本来就没跑"。
func TestStopNotRunning(t *testing.T) {
	a, _ := cliEnv(t)
	// CreateInstance 要写桌面入口，入口里的 Exec 指向 CLI；测试用固定路径。
	a.env.CliPath = "/usr/bin/appclone"
	app := "testapp"
	appsDir := filepath.Join(a.env.Resolver.XDG["DATA_HOME"], "applications")
	if err := os.MkdirAll(appsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(a.env.Resolver.Home, "testapp-bin")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appsDir, "testapp.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Test App\nExec="+execPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := core.Profile{
		App: app, Version: core.ProfileVersion, Exec: execPath,
		Paths: []string{"~/.config/TestApp"}, ProbeMethod: "manual",
	}
	if err := a.env.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := a.env.CreateInstance(context.Background(), app, profile, "main"); err != nil {
		t.Fatal(err)
	}

	cmd := newStopCmd(a)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{app, "main"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("没有在跑的分身，stop 必须报错")
	}
	if code := core.ExitCode(err); code != core.CodeState {
		t.Fatalf("stop 未运行应退出 7，得到 %d（%v）", code, err)
	}
	var ce *core.Error
	if !isCoreError(err, &ce) || ce.Key != "err.instance.not_running" {
		t.Fatalf("错误键应为 err.instance.not_running，得到 %v", err)
	}
}
