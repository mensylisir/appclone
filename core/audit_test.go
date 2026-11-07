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

	sess, layers, err := e.beginAudit(in)
	if err != nil {
		t.Fatal(err)
	}
	if layers == nil || layers.Merged == "" {
		t.Fatal("审计层必须给出 merged 路径")
	}
	// 会话还没结束：不该已经有 run.toml（那是"正常跑完"的凭证）。
	if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "run.toml")); !os.IsNotExist(err) {
		t.Fatal("run.toml 只能在应用退出后写")
	}
	if err := sess.finish(0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "run.toml")); err != nil {
		t.Fatalf("finish 应写 run.toml：%v", err)
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

// 上一轮现场还在（quarantine-*）时，不许开始新的审计：会把可能属于用户的文件覆盖掉。
func TestAuditRefusesWhenEvidencePending(t *testing.T) {
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	in, err := e.GetInstance(app, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.AuditDir(app, id), "quarantine-20260101T000000"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.beginAudit(in); ExitCode(err) != CodeEnv {
		t.Fatalf("有未消费的证据时应拒绝开始审计（3），得到 %v", err)
	}
}
