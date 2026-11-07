package core

import (
	"context"
	"os"
	"path/filepath"
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

// 审计层落地后，verify 的判定逻辑就是这一段：喂一棵 upper 树，给出结论，
// 并按结论处置证据（§6.5 留存策略）。
func TestVerifyJudgesUpperTree(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	upper := filepath.Join(e.AuditDir(app, id), "upper")
	// 预期内的写入：缓存噪声（ignored 类别本来就该进 upper）。
	writeTree(t, filepath.Join(upper, filepath.FromSlash(".cache/TestApp/blob")))
	// 有 upper 就必须有 run.toml：缺了说明上次审计没正常结束，只能给 unverified。
	writeTree(t, filepath.Join(e.AuditDir(app, id), "run.toml"))

	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictVerified || v.ExitCode() != CodeOK {
		t.Fatalf("只有 ignored 里的写入时应判 verified，得到 %s / %v", v.Conclusion, v.Violations)
	}
	// verified 之后证据被消费：upper 与 run.toml 删掉、summary 落盘。
	if _, err := os.Stat(upper); !os.IsNotExist(err) {
		t.Fatal("verified 之后 upper 应被删除（证据已消费）")
	}
	if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "run.toml")); !os.IsNotExist(err) {
		t.Fatal("verified 之后 run.toml 应被删除")
	}
	if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "summary.toml")); err != nil {
		t.Fatalf("verified 之后应留下摘要：%v", err)
	}
	// 再验一次：走"重放"，结论一致且不会被重新对账改写。
	again, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Conclusion != VerdictVerified || again.GeneratedAt == "" {
		t.Fatalf("重放应给出同一结论并带上时间，得到 %+v", again)
	}
}

// 越界：结论 8、**证据搬进 quarantine-* 而不是删掉**（越界文件就在里面）。
func TestVerifyQuarantinesOnLeak(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	audit := e.AuditDir(app, id)
	upper := filepath.Join(audit, "upper")
	writeTree(t, filepath.Join(upper, filepath.FromSlash(".config/OtherApp/state.json")))
	writeTree(t, filepath.Join(audit, "run.toml"))

	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictLeak || v.ExitCode() != CodeLeak {
		t.Fatalf("越界应判 leak-detected（8），得到 %s / %v", v.Conclusion, v.Violations)
	}
	if !v.Retained || v.Quarantine == "" {
		t.Fatalf("越界必须保留证据并给出位置，得到 %+v", v)
	}
	// 证据确实被搬到了 quarantine，而不是删掉。
	moved := filepath.Join(v.Quarantine, filepath.FromSlash(".config/OtherApp/state.json"))
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("越界文件应留在 quarantine 里：%v", err)
	}
	if _, err := os.Stat(upper); !os.IsNotExist(err) {
		t.Fatal("原来的 upper 应已被搬走")
	}
	if _, err := os.Stat(filepath.Join(audit, "summary.toml")); err != nil {
		t.Fatalf("结论应落盘：%v", err)
	}
}

// 摘出失效：现场原样保留（连 upper 都不动），结论 unverified + 退出 4。
func TestVerifyKeepsEvidenceOnExtractFailure(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	audit := e.AuditDir(app, id)
	upper := filepath.Join(audit, "upper")
	writeTree(t, filepath.Join(upper, filepath.FromSlash("Downloads/important.docx")))
	writeTree(t, filepath.Join(audit, "run.toml"))

	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictUnverified || v.Reason != "extract_failed" {
		t.Fatalf("公共目录出现在 upper 应判 extract_failed，得到 %+v", v)
	}
	if v.ExitCode() != CodeProbe {
		t.Fatalf("摘出失效的退出码应为 4，得到 %d", v.ExitCode())
	}
	if _, err := os.Stat(filepath.Join(upper, filepath.FromSlash("Downloads/important.docx"))); err != nil {
		t.Fatalf("extract_failed 时现场必须原样保留：%v", err)
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

// 摘要里的结论只认三个枚举值，且"说泄漏却没保留证据"按保留读：
// 否则手写一份 summary.toml 就能让 verify 报一个自相矛盾的结论。
func TestReadSummaryValidatesConclusion(t *testing.T) {
	e := testEnv(t)
	app, _ := seedVerifyApp(t, e)
	dir := e.AuditDir(app, "main")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "summary.toml")
	cases := []struct {
		body     string
		want     string
		retained bool
	}{
		{"conclusion = \"OK\"\nviolations = []\n", VerdictUnverified, true},
		{"conclusion = \"verified\"\n", VerdictVerified, false},
		// 摘要自称"没保留"就照实读 false：CLI 要提示"文件可能已经取不回"，
		// 而不是反过来断言"证据已保留"（那是把用户的明确记录改成相反的结论）。
		{"conclusion = \"leak-detected\"\nevidence_retained = false\n", VerdictLeak, false},
		// reason 也必须过校验：未知 reason 会让 CLI 说错"哪种不知道"。
		{"conclusion = \"unverified\"\nreason = \"whatever\"\n", VerdictUnverified, true},
	}
	for _, c := range cases {
		if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		v, ok, err := readSummary(path)
		if err != nil || !ok {
			t.Fatalf("读 %q 失败：%v", c.body, err)
		}
		if v.Conclusion != c.want {
			t.Fatalf("结论 %q 应被判成 %s，得到 %s", c.body, c.want, v.Conclusion)
		}
		if v.Retained != c.retained {
			t.Fatalf("retained 期望 %v，得到 %v（%q）", c.retained, v.Retained, c.body)
		}
		if c.body == "conclusion = \"unverified\"\nreason = \"whatever\"\n" && v.Reason != "audit_unreadable" {
			t.Fatalf("未知 reason 应降级成 audit_unreadable，得到 %q", v.Reason)
		}
	}
}

// §6.5 表格的六种证据情形都要有结论，"有没有证据"和"证据完不完整"是两件事。
// （这组断言曾在重构中被误删，现在按新口径写回：paths 也属于"本该被 bind 走"。）
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

	// ③ 只有 run.toml、upper 被删 → 证据不完整，不是"从没审计"。
	if err := os.RemoveAll(filepath.Join(audit, "upper")); err != nil {
		t.Fatal(err)
	}
	v, err = e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictUnverified || v.Reason != "audit_incomplete" {
		t.Fatalf("upper 被删应判 audit_incomplete，得到 %+v", v)
	}

	// ④ upper 已被消费，只剩 summary.toml → 重放已存结论，不重新对账。
	if err := os.WriteFile(filepath.Join(audit, "summary.toml"), []byte(
		"conclusion = \"leak-detected\"\ncoverage = [\"/home/u\"]\nviolations = [\"/home/u/.config/Other\"]\nignored = [\"~/.cache\"]\nevidence_retained = true\ngenerated_at = \"2026-11-07T00:00:00Z\"\n",
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
	if v.GeneratedAt == "" {
		t.Fatal("重放时必须带上摘要的生成时间，否则用户不知道结论有多旧")
	}
	// 摘要里的 ignored 是审计当时的名单，不许被现在的档案覆盖。
	if len(v.Ignored) != 1 || v.Ignored[0] != "~/.cache" {
		t.Fatalf("应使用摘要里的 ignored，得到 %v", v.Ignored)
	}

	// ⑤ 摘要损坏 → unverified + 保留（读不出来不代表里面没东西）。
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
