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
