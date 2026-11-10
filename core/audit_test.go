package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 审计层必须落在 ④ 与 ⑤ 之间：overlay 罩住 HOME、公共目录与 AC_BASE 绑回真实，
// 然后才是实例私有目录。顺序错了会出现两种事故：
//   - 实例 bind 在前 → 被公共目录 bind 盖掉，应用数据落进 upper，verify 误报越界；
//   - AC_BASE 不 bind → 工具自己的写入进 upper，最后一并进 quarantine。
func TestBuildBwrapArgsAuditLayerOrder(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstance(context.Background(), app, p, "main")
	if err != nil {
		t.Fatal(err)
	}
	o := e.opts()
	o.Audit = &AuditLayers{
		Merged: "/var/tmp/appclone/audit.0/" + app + ".main/merged",
		PublicBinds: []BindPair{{
			Src: filepath.Join(e.Resolver.RealHome, "Downloads"),
			Dst: filepath.Join(e.Resolver.Home, "Downloads"),
		}},
	}
	argv, err := BwrapCommand(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	order := []string{
		"--proc /proc",
		"--bind " + o.Audit.Merged + " " + e.Resolver.Home,
		"--bind " + filepath.Join(e.Resolver.RealHome, "Downloads") + " " + filepath.Join(e.Resolver.Home, "Downloads"),
		"--bind " + e.Base + " " + e.Base,
	}
	at := 0
	for _, want := range order {
		idx := strings.Index(joined[at:], want)
		if idx < 0 {
			t.Fatalf("审计层顺序不对：期望 %q 出现在 %q 之后\n实际：%s", want, joined[:at], joined)
		}
		at += idx + len(want)
	}
	// 实例私有目录（paths 的 bind）必须在审计层之后。
	instBind := "--bind " + filepath.Join(in.Root, strings.TrimPrefix(filepath.Join(e.Resolver.Home, ".config", "TestApp"), "/"))
	if idx := strings.Index(joined, instBind); idx < at {
		t.Fatalf("实例 bind 必须排在审计层之后：%s", joined)
	}
}

// 一次完整审计：跑完写 run.toml、保留写层；verify 消费掉它。
func TestAuditSessionLifecycle(t *testing.T) {
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	in, err := e.GetInstance(app, id)
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeMounter{entries: []string{".cache/TestApp/blob"}}
	e.Mounter = m
	// 已存在的 shared_writes 才会被 bind（`--bind-try`），也才会被记进凭证。
	// seedVerifyApp 在那条路径上声明了共享但可写，这里把它建出来。
	sharedDir := filepath.Join(e.Resolver.Home, ".config", "fcitx5")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}

	sess, layers, err := e.beginAudit(in, p)
	if err != nil {
		t.Fatal(err)
	}
	if layers == nil || layers.Merged == "" {
		t.Fatal("审计层必须给出 merged 路径")
	}
	// 开始时就有一条**进行中**凭证（finished_at 为空）：它顶掉上一轮 run.toml，
	// 让"新审计在写层建立前失败"也能被 verify 判成 audit_incomplete。
	rec, ok, err := readRunRecord(filepath.Join(e.AuditDir(app, id), "run.toml"))
	if err != nil || !ok {
		t.Fatalf("开始时应有进行中凭证：%v/%v", ok, err)
	}
	if rec.complete() {
		t.Fatal("应用还没退出，凭证不能是已完成状态")
	}
	if err := sess.finish(0); err != nil {
		t.Fatal(err)
	}
	// 凭证必须记下**本次实际** bind 的清单：verify 靠它区分"摘出失效"与"泄漏"。
	// 只断言文件存在的话，把 finish 里的字段赋值删掉测试仍然全绿。
	rec, ok, err = readRunRecord(filepath.Join(e.AuditDir(app, id), "run.toml"))
	if err != nil || !ok {
		t.Fatalf("读 run.toml 失败：%v/%v", ok, err)
	}
	if !rec.complete() {
		t.Fatal("finish 必须把凭证标成已完成（finished_at 非空）")
	}
	if !rec.BindsRecorded {
		t.Fatal("新格式必须带 binds_recorded 标记（否则会被回落成按档案推断）")
	}
	if len(rec.Paths) != 1 || rec.Paths[0] != filepath.Join(e.Resolver.Home, ".config", "TestApp") {
		t.Fatalf("run.toml 必须记审计当时的 paths，得到 %v", rec.Paths)
	}
	if rec.Base != e.Base {
		t.Fatalf("run.toml 必须记 AC_BASE，得到 %q", rec.Base)
	}
	if len(rec.PublicBinds) == 0 {
		t.Fatal("run.toml 必须记实际 bind 的公共目录")
	}
	foundShared := false
	for _, s := range rec.SharedBinds {
		if s == sharedDir {
			foundShared = true
		}
	}
	if !foundShared {
		t.Fatalf("run.toml 必须记实际存在的 shared_writes（%s），得到 %v", sharedDir, rec.SharedBinds)
	}

	if _, err := os.Stat(filepath.Join(e.AuditWorkDir(app, id), "upper")); err != nil {
		t.Fatalf("审计结束后必须保留写层（证据）：%v", err)
	}

	// verify 消费证据：忽略类别里的写入 → verified → 删写层 + 落摘要。
	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictVerified {
		t.Fatalf("只有 ignored 写入应判 verified，得到 %+v", v)
	}
	if _, err := os.Stat(filepath.Join(e.AuditWorkDir(app, id), "upper")); !os.IsNotExist(err) {
		t.Fatal("verified 之后写层应被消费")
	}
}

// 上一轮写层还有内容时：**搬进唯一命名的 quarantine-***，而不是拒绝开始。
// 拒绝会给用户一个出不去的死循环（取回文件后没有任何命令能消费证据）。
func TestAuditQuarantinesPendingEvidence(t *testing.T) {
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	in, err := e.GetInstance(app, id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	e.Mounter = &fakeMounter{}

	oldUpper := filepath.Join(e.AuditWorkDir(app, id), "upper")
	writeTree(t, filepath.Join(oldUpper, filepath.FromSlash(".config/OldApp/token")))
	// 上一轮的 run.toml：新审计必须用一条"进行中"凭证顶掉它，否则新审计中途
	// 崩掉时会被当成"本次正常跑完"。
	seedRunRecord(t, filepath.Join(e.AuditDir(app, id), "run.toml"), runRecord{App: app, ID: id})

	if _, _, err := e.beginAudit(in, p); err != nil {
		t.Fatalf("有残留写层时应搬走后继续，而不是拒绝：%v", err)
	}
	qs, _ := filepath.Glob(filepath.Join(e.AuditDir(app, id), "quarantine-*"))
	if len(qs) != 1 {
		t.Fatalf("残留写层应搬进 quarantine-*，得到 %v", qs)
	}
	if _, err := os.Stat(filepath.Join(qs[0], filepath.FromSlash(".config/OldApp/token"))); err != nil {
		t.Fatalf("quarantine 必须保留原文件：%v", err)
	}
	rec, ok, err := readRunRecord(filepath.Join(e.AuditDir(app, id), "run.toml"))
	if err != nil || !ok {
		t.Fatalf("新审计开始时应写进行中凭证：%v/%v", ok, err)
	}
	if rec.complete() {
		t.Fatal("新审计刚开始，凭证不能还是上一轮的已完成状态")
	}
}

// 已经有 quarantine-*（用户还没取回文件）不该阻塞新审计：证据是唯一命名的，
// 新审计写到自己的 /var/tmp 写层，二者不会互相覆盖。
func TestAuditDoesNotBlockOnExistingQuarantine(t *testing.T) {
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	in, err := e.GetInstance(app, id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	e.Mounter = &fakeMounter{}
	if err := os.MkdirAll(filepath.Join(e.AuditDir(app, id), "quarantine-20260101T000000"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.beginAudit(in, p); err != nil {
		t.Fatalf("已有 quarantine 不应阻塞新审计：%v", err)
	}
}

// 清扫只能清"真正的残留"：别的实例正在跑、或已跑完等 verify 的证据，
// 一样都不能动。早期实现按 glob 全量清，两个实例同时审计时 B 会卸掉 A 的挂载。
func TestAuditSweepSkipsPendingVerify(t *testing.T) {
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	work := e.AuditWorkDir(app, id)
	writeTree(t, filepath.Join(work, "upper", filepath.FromSlash(".config/TestApp/state")))
	seedRunRecord(t, filepath.Join(e.AuditDir(app, id), "run.toml"), runRecord{
		App: app, ID: id, BindsRecorded: true,
	})
	if err := e.sweepStaleAudits(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "upper")); err != nil {
		t.Fatalf("等 verify 的证据不能被清扫：%v", err)
	}
}

func TestAuditSweepSkipsRunningInstance(t *testing.T) {
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	work := e.AuditWorkDir(app, id)
	writeTree(t, filepath.Join(work, "upper", filepath.FromSlash(".config/TestApp/state")))
	lock, err := e.HoldRunLock(app, id)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := e.sweepStaleAudits(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "upper")); err != nil {
		t.Fatalf("正在运行的实例现场不能被清扫：%v", err)
	}
}

// 无人认领的写层才是 stale：实例已删/从没建过，清掉并保留可能含用户文件的内容。
func TestAuditSweepQuarantinesUnclaimed(t *testing.T) {
	e := probeEnv(t)
	app, _ := seedVerifyApp(t, e)
	work := e.AuditWorkDir(app, "ghost")
	writeTree(t, filepath.Join(work, "upper", filepath.FromSlash("Downloads/ghost.docx")))
	if err := e.sweepStaleAudits(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("无人认领的写层应被清走：%v", err)
	}
	qs, _ := filepath.Glob(filepath.Join(e.TmpRoot, "appclone-quarantine-*"))
	found := false
	for _, q := range qs {
		if _, err := os.Stat(filepath.Join(q, "upper", "Downloads", "ghost.docx")); err == nil {
			found = true
			os.RemoveAll(q)
		}
	}
	if !found {
		t.Fatal("现场应搬进 quarantine，而不是删掉")
	}
}

// 读不出 upper（子目录 chmod 000 / IO 错误）时，清扫必须**搬走**而不是删除：
// 看不见内容恰恰可能意味着里面有用户文件。早期实现把读错误与"空"合并处理，
// 会在下一次 run --audit 时直接删掉整棵证据。
func TestAuditSweepQuarantinesUnreadableUpper(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，构造不出读失败")
	}
	e := probeEnv(t)
	app, _ := seedVerifyApp(t, e)
	work := e.AuditWorkDir(app, "ghost")
	sub := filepath.Join(work, "upper", "sub")
	writeTree(t, filepath.Join(sub, "secret.txt"))
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 清扫回退时 000 现场仍会留在写层根；先补权再让 t.TempDir 清理，
		// 免得真正的失败原因被 RemoveAll 的权限错误盖住。
		chmodTestTree(t, e.TmpRoot)
	})

	if err := e.sweepStaleAudits(); err != nil {
		t.Fatalf("读失败不应让清扫报环境错误（它应改为保留）：%v", err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("读不出内容的写层应被搬走：%v", err)
	}
	qs, _ := filepath.Glob(filepath.Join(e.TmpRoot, "appclone-quarantine-*"))
	if len(qs) != 1 {
		t.Fatalf("应产生一个 quarantine，得到 %v", qs)
	}
	defer os.RemoveAll(qs[0])
	qsub := filepath.Join(qs[0], "upper", "sub")
	if err := os.Chmod(qsub, 0o755); err != nil {
		t.Fatalf("quarantine 里的目录应可访问：%v", err)
	}
	if _, err := os.Stat(filepath.Join(qsub, "secret.txt")); err != nil {
		t.Fatalf("读不出内容的证据也必须原样保留：%v", err)
	}
}

// 上一轮 verified 的 summary 不能在**新审计失败**后被重放成"这次通过"。
// 新审计一开始就写"进行中"凭证；它在写层建起来之前失败时，verify 必须判
// audit_incomplete，而不是拿旧摘要退出 0。
func TestNewAuditAttemptDoesNotReplayOldSummary(t *testing.T) {
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	in, err := e.GetInstance(app, id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	e.Mounter = &fakeMounter{entries: []string{".cache/TestApp/blob"}}

	// 先完成一次能判 verified 的审计，留下 summary.toml。
	sess, _, err := e.beginAudit(in, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.finish(0); err != nil {
		t.Fatal(err)
	}
	v, err := e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != VerdictVerified {
		t.Fatalf("前置审计应 verified，得到 %+v", v)
	}

	// 再开一轮，并在写层建立后立即放弃（模拟失败/被杀）。
	sess, _, err = e.beginAudit(in, p)
	if err != nil {
		t.Fatal(err)
	}
	sess.abandon(false)

	v, err = e.Verify(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion == VerdictVerified {
		t.Fatal("新审计没跑完时不能重放旧 verified（退出码 0 会被脚本当通过）")
	}
	if v.Reason != "audit_incomplete" {
		t.Fatalf("应判 audit_incomplete，得到 %+v", v)
	}
}

// 与 TestSweepOwnershipUsesProcessUID 同因：审计写层清扫也必须按进程真实
// uid 判属主，否则 CI（uid != 1000）上无人认领的写层永远留着。
func TestAuditSweepOwnershipUsesProcessUID(t *testing.T) {
	e := probeEnv(t)
	e.UID = os.Getuid() + 1
	app, _ := seedVerifyApp(t, e)
	work := e.AuditWorkDir(app, "ghost")
	writeTree(t, filepath.Join(work, "upper", filepath.FromSlash(".config/TestApp/state")))
	if err := e.sweepStaleAudits(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("本进程的无人认领写层应按真实 uid 判定属主并搬走：%v", err)
	}
}
