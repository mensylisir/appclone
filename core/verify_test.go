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
	// 预期内的写入：缓存噪声 + 共享可写。
	for _, rel := range []string{
		".cache/TestApp/blob",
		".config/fcitx5/profile",
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

// 公共目录出现在 upper 里**不是**"预期共享"：审计层把公共目录 bind 回了真实目录，
// 它出现在 upper 就说明摘出失效——那是必须报出来的第一红线。
func TestJudgeTreatsPublicDirInUpperAsLeak(t *testing.T) {
	e := testEnv(t)
	app, _ := seedVerifyApp(t, e)
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	p.SharedWrites = append(p.SharedWrites, "~/Downloads")
	v := e.Judge(p, []UpperEntry{{Path: "Downloads/important.docx", Kind: EntryFile}})
	if v.Conclusion != VerdictLeak {
		t.Fatalf("公共目录写进 upper 必须报 leak，得到 %s", v.Conclusion)
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

// 工具自己写在 AC_BASE 里的东西不算越界：否则每一次 verified 都变成 leak-detected
// （实例的 root/locks/dbus 全在 AC_BASE 下）。
func TestJudgeAllowsAcBaseWrites(t *testing.T) {
	e := testEnv(t)
	e.Base = filepath.Join(e.Resolver.Home, ".local", "share", "appclone")
	p := Profile{Ignored: DefaultIgnored()}
	v := e.Judge(p, []UpperEntry{
		{Path: ".local/share/appclone/instances/testapp/main/root/x", Kind: EntryFile},
	})
	if v.Conclusion != VerdictVerified {
		t.Fatalf("AC_BASE 下的写入不算越界，得到 %s / %v", v.Conclusion, v.Violations)
	}
}

// §6.5 表格的五种情形都要有结论，"有没有证据"和"证据完不完整"是两件事。
func TestVerifyEvidenceCompleteness(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	audit := e.AuditDir(app, id)

	// ① upper 在、run.toml 不在 → 上次审计没跑完，不能算通过，且证据必须留着。
	writeTree(t, filepath.Join(audit, "upper", ".config", "OtherApp", "x"))
	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictUnverified || v.Reason != "audit_incomplete" || !v.Retained {
		t.Fatalf("缺 run.toml 应判 unverified/audit_incomplete 且保留证据，得到 %+v", v)
	}

	// ② upper 空、run.toml 在 → 审计跑了但应用没写东西：空集不算通过。
	if err := os.RemoveAll(filepath.Join(audit, "upper")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(audit, "upper"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(audit, "run.toml"))
	v, err = e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictUnverified || v.Reason != "no_writes" {
		t.Fatalf("零写入应判 unverified/no_writes（空集不算通过），得到 %+v", v)
	}

	// ③ upper 已被消费，只剩 summary.toml → 重放已存结论，不重新对账。
	if err := os.RemoveAll(filepath.Join(audit, "upper")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(audit, "summary.toml"), []byte(
		"conclusion = \"leak-detected\"\ncoverage = [\"/home/u\"]\nviolations = [\"/home/u/.config/Other\"]\nevidence_retained = true\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err = e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictLeak || v.ExitCode() != CodeLeak {
		t.Fatalf("应重放摘要里的 leak-detected（退出 8），得到 %+v", v)
	}
	if !v.Retained {
		t.Fatal("摘要写着证据保留，重放时不能改成没保留")
	}

	// ④ 摘要损坏 → unverified + 保留（读不出来不代表里面没东西）。
	if err := os.WriteFile(filepath.Join(audit, "summary.toml"), []byte("这不是 toml {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err = e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictUnverified || v.Reason != "audit_unreadable" || !v.Retained {
		t.Fatalf("损坏的摘要应判 unverified/audit_unreadable 且保留，得到 %+v", v)
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
