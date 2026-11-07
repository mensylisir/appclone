package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedVerifyApp 建一个带档案与实例的应用，并把 ignored / shared_writes 填上，
// 因为对账口径全靠这两个字段。
func seedVerifyApp(t *testing.T, e *Env) (app, id string) {
	t.Helper()
	app, _ = seedApp(t, e)
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	p.Ignored = DefaultIgnored()
	p.SharedWrites = []string{"~/.config/fcitx5"}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	in, err := e.CreateInstance(context.Background(), app, p, "main")
	if err != nil {
		t.Fatal(err)
	}
	return app, in.ID
}

// 没有证据时必须给 unverified，而且是**退出 0**：unverified 是"不知道"，
// 不是"失败"，更不是"通过"。
func TestVerifyWithoutEvidenceIsUnverified(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictUnverified {
		t.Fatalf("无证据应判 unverified，得到 %s", v.Conclusion)
	}
	if v.Reason != "audit_not_enabled" {
		t.Fatalf("必须给出原因（视图层要显示它），得到 %q", v.Reason)
	}
	if v.ExitCode() != CodeOK {
		t.Fatalf("unverified 的退出码应为 0，得到 %d", v.ExitCode())
	}
	if len(v.Violations) != 0 {
		t.Fatalf("没有证据却报越界：%v", v.Violations)
	}
}

// 实例在跑时读到的 upper 是半截证据，必须先拒绝（退出 7）。
func TestVerifyRefusesWhileRunning(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	lock, err := e.HoldRunLock(app, id)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := e.Verify(app, id); ExitCode(err) != CodeState {
		t.Fatalf("运行中 verify 应被拒（7），得到 %v", err)
	}
}

// 审计层落地后，verify 的判定逻辑就是这一段：喂一棵 upper 树，给出结论。
func TestVerifyJudgesUpperTree(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	upper := filepath.Join(e.AuditDir(app, id), "upper")
	// 预期内的写入：缓存噪声（ignored 类别本来就该进 upper）。
	for _, rel := range []string{
		".cache/TestApp/blob",
	} {
		writeTree(t, filepath.Join(upper, filepath.FromSlash(rel)))
	}
	// 有 upper 就必须有 run.toml：缺了说明上次审计没正常结束，只能给 unverified。
	writeTree(t, filepath.Join(e.AuditDir(app, id), "run.toml"))
	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictVerified || v.ExitCode() != CodeOK {
		t.Fatalf("只有预期写入时应判 verified，得到 %s / %v", v.Conclusion, v.Violations)
	}

	// 再写一条越界：别的应用的配置目录。
	writeTree(t, filepath.Join(upper, ".config", "OtherApp", "state.json"))
	v, err = e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictLeak || v.ExitCode() != CodeLeak {
		t.Fatalf("越界应判 leak-detected（8），得到 %s", v.Conclusion)
	}
	if len(v.Violations) != 1 || !strings.HasSuffix(v.Violations[0], filepath.FromSlash(".config/OtherApp/state.json")) {
		t.Fatalf("越界清单应只报那条真实写入（不含被顺带造出的父目录）：%v", v.Violations)
	}
	if !v.Retained {
		t.Fatal("越界时证据必须保留（删掉等于销毁用户数据）")
	}
}

// 公共目录 / AC_BASE / shared_writes 本该被 bind 回真实位置，出现在 upper 里
// 说明"摘出"没生效：此时任何结论都不算数（退出 4 + 保留现场），**不能**说
// verified，也不能只说 leak——因为下一步完全不同（先找回文件，别清理）。
func TestJudgeTreatsExtractFailureAsVoid(t *testing.T) {
	e := testEnv(t)
	app, _ := seedVerifyApp(t, e)
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	p.SharedWrites = append(p.SharedWrites, "~/Downloads")
	cases := []UpperEntry{
		{Path: "Downloads/important.docx", Kind: EntryFile}, // 公共目录
		{Path: ".config/fcitx5/profile", Kind: EntryFile},   // shared_writes
	}
	for _, en := range cases {
		v := e.Judge(p, []UpperEntry{en})
		if v.Conclusion != VerdictUnverified || v.Reason != "extract_failed" {
			t.Fatalf("%s 出现在 upper 里应判 extract_failed，得到 %s/%s", en.Path, v.Conclusion, v.Reason)
		}
		if v.ExitCode() != CodeProbe {
			t.Fatalf("摘出失效的退出码应为 4，得到 %d", v.ExitCode())
		}
		if !v.Retained {
			t.Fatal("摘出失效时绝不能删证据")
		}
	}
}

// AC_BASE 下的写入同样是"摘出失效"，不是普通越界。
func TestJudgeTreatsAcBaseInUpperAsExtractFailure(t *testing.T) {
	e := testEnv(t)
	e.Base = filepath.Join(e.Resolver.Home, ".local", "share", "appclone")
	p := Profile{Ignored: DefaultIgnored()}
	v := e.Judge(p, []UpperEntry{
		{Path: ".local/share/appclone/instances/testapp/main/root/x", Kind: EntryFile},
	})
	if v.Conclusion != VerdictUnverified || v.Reason != "extract_failed" {
		t.Fatalf("AC_BASE 写进 upper 说明实例目录没 bind 上，应判 extract_failed，得到 %s/%s",
			v.Conclusion, v.Reason)
	}
}

// 同一路径多条条目只报一次：用户看的是"哪些路径越界"，不是条目数。
func TestJudgeDeduplicatesViolations(t *testing.T) {
	e := testEnv(t)
	app, _ := seedVerifyApp(t, e)
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	v := e.Judge(p, []UpperEntry{
		{Path: ".config/OtherApp/a", Kind: EntryFile},
		{Path: ".config/OtherApp/a", Kind: EntryFile},
		{Path: ".config/OtherApp/b", Kind: EntryFile},
	})
	if len(v.Violations) != 2 {
		t.Fatalf("应去重成 2 条，得到 %v", v.Violations)
	}
}

// writeTree 造一条文件（连同父目录）。
func writeTree(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}
