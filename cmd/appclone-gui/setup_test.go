package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mensylisir/appclone/core"
)

// buildProfile 必须走 DeriveProfile：父 path + 子凭据要同时落进档案，
// 非路径字段（isolate_dbus/env/lock_files）不能被界面重算抹掉。
func TestBuildProfileKeepsNonPathFieldsAndCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	e, err := core.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	r := e.Resolver
	u := &ui{env: e, lang: "zh_CN"}
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "demo")
	cred := filepath.Join(cfg, "credentials.json")
	v := &setupView{
		u: u, appKey: "demo",
		profile: core.Profile{
			App: "demo", ProbeMethod: "overlay", ReadTrace: core.ReadTraceOK,
			IsolateDBus: true,
			Env:         map[string]string{"FOO": "1"},
			LockFiles:   []string{"~/.config/demo/lock"},
			Ignored:     core.DefaultIgnored(),
		},
		cands: []core.Candidate{{
			Path: cfg, Class: core.ClassConfig, DisplayClass: core.ClassConfig,
			Checked: true, Credential: core.CredentialStrong, CredentialPath: cred,
		}},
	}
	p := v.buildProfile()
	if !p.IsolateDBus || p.Env["FOO"] != "1" || len(p.LockFiles) != 1 {
		t.Fatalf("非路径字段必须保留：%+v", p)
	}
	if len(p.Paths) != 1 || !samePathForTest(p.Paths[0], cfg, r) {
		t.Fatalf("父路径应进 paths：%v", p.Paths)
	}
	if len(p.CredentialPaths) != 1 || !samePathForTest(p.CredentialPaths[0], cred, r) {
		t.Fatalf("子凭据应进 credential_paths：%v", p.CredentialPaths)
	}
	if p.ReadTrace != core.ReadTraceOK {
		t.Fatalf("read_trace 必须保留：%q", p.ReadTrace)
	}
}

func samePathForTest(store, abs string, r *core.PathResolver) bool {
	got, err := r.Expand(store)
	return err == nil && filepath.Clean(got) == filepath.Clean(abs)
}

// 确认页横幅的四态与 run-scoped 告警：每个状态都要有独立文案，
// 且探测中不能把零值 read_trace 误报成"旧档案"。
func TestReadsBannerPartsStates(t *testing.T) {
	u := &ui{lang: "zh_CN"}
	cases := map[string]string{
		core.ReadTraceOK:          "ui.setup.reads.ok",
		core.ReadTracePartial:     "ui.setup.reads.partial",
		core.ReadTraceUnavailable: "ui.setup.reads.unavailable",
		"":                        "ui.setup.reads.legacy",
	}
	for rt, key := range cases {
		v := &setupView{u: u, state: "confirm", readTrace: rt}
		parts := v.readsBannerParts()
		if len(parts) != 1 || parts[0] != u.tr(key, nil) {
			t.Fatalf("read_trace=%q 横幅应为 %q，得到 %v", rt, u.tr(key, nil), parts)
		}
	}

	// 探测中（state=probing）还没拿到四态：不能先报"旧档案"。
	v := &setupView{u: u, state: "probing"}
	if parts := v.readsBannerParts(); len(parts) != 0 {
		t.Fatalf("探测中不应渲染四态横幅：%v", parts)
	}

	// 被丢弃的旧凭据路径与新增告警键都要出现在横幅里。
	v = &setupView{
		u: u, state: "confirm", readTrace: core.ReadTracePartial,
		profile: core.Profile{
			Warnings:          []string{"credential_rows_truncated"},
			CredentialDropped: []string{"~/Downloads/token"},
		},
	}
	joined := strings.Join(v.readsBannerParts(), " · ")
	if !strings.Contains(joined, u.tr("probe.profile.credential_rows_truncated", nil)) {
		t.Fatalf("凭据行超限告警必须可见：%s", joined)
	}
	if !strings.Contains(joined, "~/Downloads/token") {
		t.Fatalf("被丢弃的旧凭据路径必须可见：%s", joined)
	}
}

func TestMergeWarnKeys(t *testing.T) {
	got := mergeWarnKeys([]string{"profile_conflict", "cache_credential"},
		[]string{"cache_credential", "cache_path_dropped"})
	want := []string{"profile_conflict", "cache_credential", "cache_path_dropped"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("告警合并去重/顺序错：%v", got)
	}
}

// reader_stuck 是进程级不可恢复态：错误页不能再挂「重试」，否则用户会
// 在同一个必定失败的入口上循环。
func TestRetryActionReaderStuck(t *testing.T) {
	v := &setupView{u: &ui{lang: "zh_CN"}}
	if v.retryAction(&core.Error{Key: "err.probe.reader_stuck"}) != nil {
		t.Fatal("reader_stuck 不能挂重试")
	}
	if v.retryAction(errors.New("other")) == nil {
		t.Fatal("其它探测错误应保留重试")
	}
}
