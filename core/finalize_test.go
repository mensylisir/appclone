package core

import (
	"errors"
	"path/filepath"
	"testing"
)

// 只有缓存/共享/凭证写入时不能生成空档案：判定看"会进档案的路径数"，
// 不是候选数（§6.3）。CLI 与 GUI 靠同一个错误拒绝，退出码/文案才对得上。
//
// observed 必须是**预填后**的候选数：模板补进来的灰色路径虽然进不了 paths，
// 用户看到的"观测到 N 个写入条目"要能对上确认页上的行数。
// 降级确认页只允许"读证据确实不可用 + 至少一条能进 paths 的行"；
// 否则空确认页会把用户卡死（§11.4）。
func TestFinalizeProbeAllowEmpty(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "Demo")
	cands := []Candidate{{Path: cfg, Class: ClassConfig, Checked: false}}
	res := ProbeResult{Candidates: cands, ReadTrace: ReadTraceUnavailable}

	p, _, _, err := e.FinalizeProbe("demo", res, ProbeFinalize{AllowEmpty: true})
	if err != nil {
		t.Fatalf("读证据不可用且有可勾选行时应允许空档案：%v", err)
	}
	if len(p.Paths) != 0 {
		t.Fatalf("未勾选时 paths 应为空：%v", p.Paths)
	}

	// 读证据正常时 AllowEmpty 不生效：零写入仍然是错误。
	res.ReadTrace = ReadTraceOK
	if _, _, _, err := e.FinalizeProbe("demo", res, ProbeFinalize{AllowEmpty: true}); errKey(err) != "err.probe.no_writes" {
		t.Fatalf("读证据可用时不能走降级确认页：%v", err)
	}

	// 没有任何能进 paths 的行：空确认页无路可走，必须报 no_writes。
	res.ReadTrace = ReadTraceUnavailable
	res.Candidates = []Candidate{{
		Path: filepath.Join(r.XDG["CACHE_HOME"], "demo"), Class: ClassCache, Locked: true,
	}}
	if _, _, _, err := e.FinalizeProbe("demo", res, ProbeFinalize{AllowEmpty: true}); errKey(err) != "err.probe.no_writes" {
		t.Fatalf("没有可勾选行时不能进入空确认页：%v", err)
	}
}

func TestFinalizeProbeRefusesEmptyProfile(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	res := ProbeResult{Candidates: []Candidate{
		{Path: filepath.Join(r.XDG["CONFIG_HOME"], "Shared"), Class: ClassShared},
		{Path: filepath.Join(r.XDG["DATA_HOME"], "keyrings", "k.keyring"), Class: ClassKeyring},
	}}
	// 模板给一条缓存路径：PrefillCandidates 会补进候选，但它不勾选、
	// ProfileFromCandidates 不会放进 paths——零写入结论不变，observed +1。
	opt := ProbeFinalize{
		Bundled:    Profile{Paths: []string{filepath.Join(r.XDG["CACHE_HOME"], "PrefillApp")}},
		HasBundled: true,
	}
	_, cands, prefilled, err := e.FinalizeProbe("demo", res, opt)
	if err == nil {
		t.Fatal("零写入必须报错")
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.no_writes" {
		t.Fatalf("应为 err.probe.no_writes，得到 %v", err)
	}
	if prefilled != 1 || len(cands) != 3 {
		t.Fatalf("预填应补进 1 条候选（共 3 条）：prefilled=%d cands=%+v", prefilled, cands)
	}
	if got, ok := ce.Args["observed"].(int); !ok || got != len(cands) {
		t.Fatalf("observed 应含预填候选（%d），得到 %v", len(cands), ce.Args["observed"])
	}
}

// 探测一条路径都没跟踪到、结果全靠模板预填时：来源标 manual（不能冒充本机
// 探测），路径取自预填候选，prefilled 计数供确认页说明来源。
func TestFinalizeProbePrefillIsManual(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "PrefillApp")
	res := ProbeResult{Profile: Profile{ProbeMethod: "overlay", Exec: "/usr/bin/app"}}
	opt := ProbeFinalize{
		Bundled:    Profile{Paths: []string{cfg}},
		HasBundled: true,
	}

	p, cands, prefilled, err := e.FinalizeProbe("demo", res, opt)
	if err != nil {
		t.Fatal(err)
	}
	if prefilled != 1 || len(cands) != 1 {
		t.Fatalf("应预填 1 条候选：prefilled=%d cands=%+v", prefilled, cands)
	}
	if p.ProbeMethod != "manual" {
		t.Fatalf("纯模板预填必须标 manual，得到 %q", p.ProbeMethod)
	}
	if len(p.Paths) != 1 || p.Paths[0] != cfg {
		t.Fatalf("路径应来自预填候选：%v", p.Paths)
	}
	if p.Exec != "/usr/bin/app" {
		t.Fatalf("探测给的 exec 必须保留：%q", p.Exec)
	}
}

// 探测有真实写入时来源保持 overlay；模板路径补进确认页，但不改来源。
func TestFinalizeProbeKeepsProbeMethod(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "ProbeApp")
	tmplCfg := filepath.Join(r.XDG["CONFIG_HOME"], "TemplateApp")
	res := ProbeResult{
		Profile:    Profile{ProbeMethod: "overlay", Paths: []string{r.ToStoreForm(cfg)}},
		Candidates: []Candidate{{Path: cfg, Class: ClassConfig, Checked: true, Why: "probe.why.config"}},
	}
	opt := ProbeFinalize{
		Bundled:    Profile{Paths: []string{tmplCfg}},
		HasBundled: true,
	}

	p, cands, prefilled, err := e.FinalizeProbe("demo", res, opt)
	if err != nil {
		t.Fatal(err)
	}
	if p.ProbeMethod != "overlay" {
		t.Fatalf("本机探测结果不能因为预填改标来源：%q", p.ProbeMethod)
	}
	if prefilled != 1 || len(cands) != 2 {
		t.Fatalf("候选应包含探测 + 预填共 2 条：prefilled=%d cands=%+v", prefilled, cands)
	}
	if len(p.Paths) != 2 {
		t.Fatalf("预填路径也要进档案：%v", p.Paths)
	}
}

// 非路径字段以既有档案为准（用户上次确认过的配置），模板只是次一级基线；
// `ignored = []` 这类显式声明必须原样保留（§5.5）。
func TestFinalizeProbeExistingBeatsBundled(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "App")
	res := ProbeResult{
		Profile:    Profile{ProbeMethod: "overlay"},
		Candidates: []Candidate{{Path: cfg, Class: ClassConfig, Checked: true}},
	}
	opt := ProbeFinalize{
		Bundled:     Profile{Exec: "/bundled/bin", Env: map[string]string{"FROM": "bundled"}},
		HasBundled:  true,
		Existing:    Profile{Exec: "/user/bin", Env: map[string]string{"FROM": "existing"}, IsolateDBus: true, Ignored: []string{}},
		HasExisting: true,
	}

	p, _, _, err := e.FinalizeProbe("demo", res, opt)
	if err != nil {
		t.Fatal(err)
	}
	if p.Exec != "/user/bin" || p.Env["FROM"] != "existing" {
		t.Fatalf("既有档案应优先于模板：exec=%q env=%v", p.Exec, p.Env)
	}
	if !p.IsolateDBus {
		t.Fatal("既有档案的 isolate_dbus 必须保留")
	}
	if p.Ignored == nil || len(p.Ignored) != 0 {
		t.Fatalf("ignored = [] 是显式声明，不能被换回默认表：%v", p.Ignored)
	}
}

// 没有既有档案时模板当基线；连模板都没有就保持探测产物（含默认噪声表）。
func TestFinalizeProbeBundledAsBase(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "App")
	res := ProbeResult{
		Profile:    Profile{ProbeMethod: "overlay", Ignored: DefaultIgnored()},
		Candidates: []Candidate{{Path: cfg, Class: ClassConfig, Checked: true}},
	}
	p, _, _, err := e.FinalizeProbe("demo", res, ProbeFinalize{
		Bundled:    Profile{Env: map[string]string{"FROM": "bundled"}},
		HasBundled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Env["FROM"] != "bundled" {
		t.Fatalf("无既有档案时模板是基线：env=%v", p.Env)
	}

	p, _, _, err = e.FinalizeProbe("demo", res, ProbeFinalize{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Ignored == nil {
		t.Fatal("探测产物自带的默认噪声表不能丢")
	}
}

// 旧档案里属于公共目录/keyring 的凭据被丢弃时，原因应报"该范围不支持"，
// 而不是"范围收窄"（§8.4）。
func TestDeriveProfileDroppedCredentialScopeWarn(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "Demo")
	pubCred := filepath.Join(r.Home, "Downloads", "token")
	base := Profile{App: "demo", CredentialPaths: []string{r.ToStoreForm(pubCred)}}
	p := r.DeriveProfile(Profile{App: "demo"}, &base, "overlay", ReadTraceOK,
		[]Candidate{{Path: cfg, Class: ClassConfig, Checked: true}})
	if !containsString(p.Warnings, "credential_scope_unsupported") {
		t.Fatalf("公共目录凭据应报 unsupported：%v", p.Warnings)
	}
	if len(p.CredentialPaths) != 0 {
		t.Fatalf("公共目录凭据不能留下：%v", p.CredentialPaths)
	}
}

// 本次探测的 merge 告警（cache 凭据 / 凭据行超限）必须随档案交给展示层；
// 但既有档案里的旧告警不能在重探时复活。
func TestFinalizeProbeKeepsRunWarningsOnly(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "Demo")
	cands := []Candidate{{Path: cfg, Class: ClassConfig, Checked: true}}
	res := ProbeResult{
		Candidates: cands,
		ReadTrace:  ReadTraceOK,
		Profile: Profile{
			App: "demo", ProbeMethod: "overlay", Paths: []string{cfg},
			Warnings: []string{"cache_credential", "credential_rows_truncated"},
		},
	}
	p, _, _, err := e.FinalizeProbe("demo", res, ProbeFinalize{})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"cache_credential", "credential_rows_truncated"} {
		if !containsString(p.Warnings, w) {
			t.Fatalf("探测告警 %q 必须保留，得到 %v", w, p.Warnings)
		}
	}

	// 重探时 base 里有过期告警，但本次探测没再产生它 → 不得复活。
	res.Profile.Warnings = nil
	p, _, _, err = e.FinalizeProbe("demo", res, ProbeFinalize{
		Existing:    Profile{App: "demo", Warnings: []string{"cache_credential"}},
		HasExisting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(p.Warnings, "cache_credential") {
		t.Fatalf("旧档案告警不得在重探后复活：%v", p.Warnings)
	}
}

// 确认门禁判据：公共目录即使被界面勾上也不算数；cache/locked 同理。
func TestCountsTowardGate(t *testing.T) {
	if CountsTowardGate(Candidate{Class: ClassConfig, DisplayClass: ClassPublic}, true) {
		t.Fatal("公共目录不能点亮确认按钮")
	}
	if CountsTowardGate(Candidate{Class: ClassConfig, Locked: true}, true) {
		t.Fatal("锁定行不能点亮确认按钮")
	}
	if CountsTowardGate(Candidate{Class: ClassConfig}, false) {
		t.Fatal("未勾选的行不能点亮确认按钮")
	}
	if !CountsTowardGate(Candidate{Class: ClassConfig}, true) {
		t.Fatal("普通配置行应计入门禁")
	}
}
