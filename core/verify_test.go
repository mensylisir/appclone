package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
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
	seedRunRecord(t, filepath.Join(e.AuditDir(app, id), "run.toml"), runRecord{App: app, ID: id})

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
	seedRunRecord(t, filepath.Join(audit, "run.toml"), runRecord{App: app, ID: id})

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

// 摘出失效：现场搬进 quarantine（不删，里面可能是用户文件），结论 unverified + 退出 4。
func TestVerifyKeepsEvidenceOnExtractFailure(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	audit := e.AuditDir(app, id)
	upper := filepath.Join(audit, "upper")
	writeTree(t, filepath.Join(upper, filepath.FromSlash("Downloads/important.docx")))
	seedRunRecord(t, filepath.Join(audit, "run.toml"), runRecord{
		App: app, ID: id,
		// 本次审计实际 bind 了 Downloads（这是摘出清单里的条目）。
		PublicBinds:   []string{filepath.Join(e.Resolver.Home, "Downloads")},
		BindsRecorded: true,
	})

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
	if !v.Retained || v.Quarantine == "" {
		t.Fatalf("extract_failed 必须保留证据并给出位置，得到 %+v", v)
	}
	// 现场搬进唯一命名的 quarantine：既不丢文件，也不阻塞下一次审计。
	if _, err := os.Stat(filepath.Join(v.Quarantine, filepath.FromSlash("Downloads/important.docx"))); err != nil {
		t.Fatalf("extract_failed 的文件应留在 quarantine 里：%v", err)
	}
	if _, err := os.Stat(upper); !os.IsNotExist(err) {
		t.Fatal("搬进 quarantine 后原 upper 不应留在审计工作区")
	}
	if _, err := os.Stat(filepath.Join(audit, "summary.toml")); err != nil {
		t.Fatalf("结论应落盘：%v", err)
	}
	// 重放：证据已搬走，摘要必须能让用户找回文件位置。
	again, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Quarantine != v.Quarantine || !again.Retained {
		t.Fatalf("重放应保留 quarantine 位置，得到 %+v", again)
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

// seedRunRecord 写一份**可解析且已完成**的 run.toml。verify 现在会读它拿实际
// bind 清单，继续用 "x" 当 fixture 会让所有带 run.toml 的用例报解析错误。
//
// 默认补 FinishedAt：凭证"完成与否"只看它；不补的话所有既有用例都会被判成
// "审计没跑完"，测的就不是原本要测的东西了。需要构造"进行中/未完成"的用例
// 显式设置 StartedAt 并留空 FinishedAt。
func seedRunRecord(t *testing.T, path string, rec runRecord) {
	t.Helper()
	if rec.Version == 0 {
		rec.Version = ProfileVersion
	}
	if rec.FinishedAt == "" && rec.StartedAt == "" {
		rec.FinishedAt = "2026-01-01T00:00:00Z"
	}
	data, err := toml.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// verify 必须自己校验档案：ignored=["~"] 的档案能让 Judge 对任何越界都说
// verified。只在 add/run 入口校验，等于把防线放在用户不会再走一遍的地方。
func TestVerifyRejectsBlanketAllowList(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	p.Ignored = []string{"~"}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Verify(app, id); ExitCode(err) != CodeInput {
		t.Fatalf("ignored=[~] 的档案必须被 verify 拒绝（2），得到 %v", err)
	}
}

// unsupported 与 ignored/shared_writes 共用放行名单，必须接受同一套 blanket 校验。
func TestValidateProfileRejectsBlanketUnsupported(t *testing.T) {
	e := testEnv(t)
	p := Profile{Exec: "/bin/true", Paths: []string{"~/.config/TestApp"}}
	for _, home := range []string{"~", "/"} {
		p.Unsupported = []Unsupported{{Kind: "keyring", Path: home}}
		err := ValidateProfile(p, e.BuildOptions())
		if err == nil || errKey(err) != "err.profile.blanket_path" {
			t.Fatalf("unsupported=[%q] 应被拒，得到 %v", home, err)
		}
	}
}

// 纵深防御：即使档案没经过 ValidateProfile（旧版本写下、或调用方绕过），
// Judge 也不能把 HOME 当成白名单。否则这就是一条"任何越界都 verified"的捷径。
func TestJudgeIgnoresBlanketAllowList(t *testing.T) {
	e := testEnv(t)
	p := Profile{Paths: []string{"~/.config/TestApp"}, Ignored: []string{"~"}}
	v := e.Judge(p, []UpperEntry{
		{Path: ".config/OtherApp/state.json", Kind: EntryFile},
	})
	if v.Conclusion != VerdictLeak {
		t.Fatalf("ignored=[~] 不得放行越界，应判 leak，得到 %s/%s", v.Conclusion, v.Reason)
	}
}

// "本该被 bind 走"的集合必须来自本次审计**实际** bind 的清单，而不是按配置
// 推断：没配置的公共目录（这里只有 Documents 被 bind）出现在 upper 是泄漏，
// 不是摘出失效——后者会让整次审计作废，且下一步完全不同。
func TestJudgeUsesActualAuditBinds(t *testing.T) {
	e := testEnv(t)
	p := Profile{Paths: []string{"~/.config/TestApp"}}
	home := e.Resolver.Home
	rec := runRecord{PublicBinds: []string{filepath.Join(home, "Documents")}, BindsRecorded: true}

	if v := e.judge(p, []UpperEntry{{Path: "Downloads/file.docx", Kind: EntryFile}}, &rec); v.Conclusion != VerdictLeak {
		t.Fatalf("未参与本次 bind 的 Downloads 应是泄漏，得到 %s/%s", v.Conclusion, v.Reason)
	}
	if v := e.judge(p, []UpperEntry{{Path: "Documents/file.docx", Kind: EntryFile}}, &rec); v.Reason != "extract_failed" {
		t.Fatalf("实际 bind 过的 Documents 出现在 upper 应判 extract_failed，得到 %s/%s", v.Conclusion, v.Reason)
	}
}

// overlay 的 opaque 标记不是用户文件，也不能被去前缀成不存在的 `.wh..opq`。
func TestReadUpperTreeSkipsOpaqueMarker(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, ".config", ".wh..wh..opq"))
	writeTree(t, filepath.Join(dir, ".config", ".wh.deleted-file"))
	entries, present, err := readUpperTree(dir)
	if err != nil || !present {
		t.Fatalf("readUpperTree: %v / present=%v", err, present)
	}
	var paths []string
	for _, en := range entries {
		paths = append(paths, en.Path)
	}
	joined := filepath.ToSlash(filepath.Join(paths...))
	if strings.Contains(joined, "opq") {
		t.Fatalf("opaque 标记不该出现在条目里：%v", paths)
	}
	found := false
	for _, en := range entries {
		if en.Kind == EntryWhiteout && en.Path == ".config/deleted-file" {
			found = true
		}
	}
	if !found {
		t.Fatalf("普通 .wh. 删除应记录为 whiteout：%v", entries)
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
	seedRunRecord(t, filepath.Join(audit, "run.toml"), runRecord{App: app, ID: id})
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

// 老格式 run.toml（写入器第一版，没有 binds_recorded）不能把 extract_failed
// 判成 leak：两者的下一步完全相反（取回文件 vs 重新探测）。
func TestJudgeLegacyRunRecordFallsBackToProfile(t *testing.T) {
	e := testEnv(t)
	p := Profile{Paths: []string{"~/.config/TestApp"}}
	rec := runRecord{} // 老文件：字段全空
	v := e.judge(p, []UpperEntry{{Path: "Downloads/file.docx", Kind: EntryFile}}, &rec)
	if v.Reason != "extract_failed" {
		t.Fatalf("老格式应按档案回退（Downloads 是公共目录）、判 extract_failed，得到 %s/%s", v.Conclusion, v.Reason)
	}
}

// 声明过、但审计开始时不存在因此没被 bind 的 shared_writes：应用在审计期间
// 把它建出来不该算越界（§6.5：命中 shared_writes 算通过）。
func TestJudgeAllowsDeclaredButUnboundSharedWrite(t *testing.T) {
	e := testEnv(t)
	p := Profile{
		Paths:        []string{"~/.config/TestApp"},
		SharedWrites: []string{"~/.config/fcitx5"},
	}
	rec := runRecord{BindsRecorded: true}
	v := e.judge(p, []UpperEntry{{Path: ".config/fcitx5/profile", Kind: EntryFile}}, &rec)
	if v.Conclusion != VerdictVerified {
		t.Fatalf("声明的 shared_writes 不该判越界，得到 %s/%s %v", v.Conclusion, v.Reason, v.Violations)
	}
}

// run.toml 写坏时降级为"证据不完整"，不能报环境错误（那样用户没有出路）。
func TestVerifyCorruptRunRecordIsIncomplete(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	audit := e.AuditDir(app, id)
	writeTree(t, filepath.Join(audit, "upper", filepath.FromSlash(".config/TestApp/x")))
	if err := os.WriteFile(filepath.Join(audit, "run.toml"), []byte("这不是 toml {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictUnverified || v.Reason != "audit_incomplete" || !v.Retained {
		t.Fatalf("写坏的凭证应判 audit_incomplete 且保留，得到 %+v", v)
	}
}

// upper 已被搬走（或误删）时，结论必须告诉用户 quarantine 在哪——否则
// "证据已保留"只是半句话。反过来，upper 还在工作区时不能指向旧 quarantine。
func TestVerifyAuditIncompletePointsToQuarantine(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e)
	audit := e.AuditDir(app, id)
	seedRunRecord(t, filepath.Join(audit, "run.toml"), runRecord{App: app, ID: id})
	q := filepath.Join(audit, "quarantine-20260101T000000-abcd")
	writeTree(t, filepath.Join(q, filepath.FromSlash("Downloads/old.docx")))

	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Reason != "audit_incomplete" || !v.Retained {
		t.Fatalf("应判 audit_incomplete + retained，得到 %+v", v)
	}
	if v.Quarantine != q {
		t.Fatalf("必须给出 quarantine 的真实路径 %s，得到 %q", q, v.Quarantine)
	}

	// 反例：upper 还在工作区（无 run.toml 的崩溃现场）时，别指向旧 quarantine。
	writeTree(t, filepath.Join(audit, "upper", filepath.FromSlash(".config/TestApp/x")))
	if err := os.Remove(filepath.Join(audit, "run.toml")); err != nil {
		t.Fatal(err)
	}
	v, err = e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Reason != "audit_incomplete" || v.Quarantine != "" {
		t.Fatalf("现场在工作区时不应指向旧 quarantine，得到 %+v", v)
	}
}

// ADR-033 升级路径：新根没有证据时，verify 必须还能读到旧根 /var/tmp 下
// 已完成、未消费的审计写层，否则升级后结论会退化成"证据不完整"。
func TestUpperDirFallsBackToLegacyRoot(t *testing.T) {
	old := legacyTmpRoot
	legacyTmpRoot = t.TempDir()
	t.Cleanup(func() { legacyTmpRoot = old })

	e := probeEnv(t)
	legacy := filepath.Join(legacyTmpRoot, "appclone",
		fmt.Sprintf("audit.%d", e.UID), "demo.main", "upper")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	// 生产形态：TmpRoot 为空才会去看旧根。
	e.TmpRoot = ""
	if got := e.upperDir("demo", "main"); got != legacy {
		t.Fatalf("应回退到旧根 %s，得到 %s", legacy, got)
	}
	// 新根一旦有 upper，旧根不得再被选中（新证据优先）。
	e.TmpRoot = t.TempDir()
	fresh := filepath.Join(e.AuditWorkDir("demo", "main"), "upper")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := e.upperDir("demo", "main"); got != fresh {
		t.Fatalf("新根有证据时应优先新根，得到 %s", got)
	}
}
