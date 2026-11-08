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
