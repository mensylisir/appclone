//go:build linux && gtk_e2e

// 这个包 import puregotk，init 阶段会 dlopen GTK：无 GTK 运行库的机器上
// `go test ./...`（CI 的 Go 门禁）会直接 panic，而不是 skip。纯逻辑单测也
// 只能和 GTK 闸门一起跑（design/guitest-gtk.sh），不能留在默认门禁里。
package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

func TestGtkProbeProgress(t *testing.T) {
	gtkTestEnv(t, "progress")
	gtk.Init()
	for _, lang := range []string{"zh_CN", "en"} {
		u := &ui{lang: lang}
		v := &setupView{u: u, state: "probing", probeSeq: 1,
			roundLabel: gtk.NewLabel(""), countLabel: gtk.NewLabel(""), retryLabel: gtk.NewLabel("retry")}
		p := core.ProbeProgress{Stage: "round_running", Round: 1, Rounds: 2,
			Elapsed: 65 * time.Second, Reads: 42, ReadAvailable: true}
		v.applyProbeProgress(1, p)
		if !strings.Contains(v.roundLabel.GetText(), "01:05") || !strings.Contains(v.countLabel.GetText(), "42") {
			t.Fatalf("%s progress not rendered: %q %q", lang, v.roundLabel.GetText(), v.countLabel.GetText())
		}
		if v.retryLabel.GetText() != "retry" {
			t.Fatal("heartbeat erased retry explanation")
		}
		p.Round, p.Elapsed, p.ReadAvailable = 2, 0, false
		v.applyProbeProgress(1, p)
		if !strings.Contains(v.roundLabel.GetText(), "2/2") || !strings.Contains(v.roundLabel.GetText(), "00:00") || v.countLabel.GetText() != u.tr("ui.setup.probing.writes_pending", nil) {
			t.Fatal("round reset or unavailable evidence is misleading")
		}
		p.Stage = "round_stopped"
		v.applyProbeProgress(1, p)
		want := u.tr("ui.setup.probing.collecting", map[string]any{"round": 2, "rounds": 2})
		if v.roundLabel.GetText() != want {
			t.Fatal("missing collecting state")
		}
		p.Stage = "round_running"
		v.probeSeq = 2
		v.applyProbeProgress(1, p)
		if v.roundLabel.GetText() != want {
			t.Fatal("stale retry callback applied")
		}
		v.state = "confirm"
		v.applyProbeProgress(2, p)
		if v.roundLabel.GetText() != want {
			t.Fatal("heartbeat changed completed page")
		}
		v.state, v.done = "probing", true
		v.applyProbeProgress(2, p)
		if v.roundLabel.GetText() != want {
			t.Fatal("heartbeat changed canceled page")
		}
		v.roundLabel.Unref()
		v.countLabel.Unref()
		v.retryLabel.Unref()
	}
}

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
			Warnings:          []string{"credential_rows_truncated", "exec_args_preserved"},
			CredentialDropped: []string{"~/Downloads/token"},
		},
	}
	joined := strings.Join(v.readsBannerParts(), " · ")
	if !strings.Contains(joined, u.tr("probe.profile.credential_rows_truncated", nil)) {
		t.Fatalf("凭据行超限告警必须可见：%s", joined)
	}
	if !strings.Contains(joined, u.tr("probe.profile.exec_args_preserved", nil)) {
		t.Fatalf("启动参数沿用告警必须可见：%s", joined)
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

// F3：保存档案只校验能力，绝不预先建 keyring 目录或密码。目录/密码属于
// 运行期（EnsureKeyring）；保存阶段替用户造状态，会让「取消创建」或
// 「保存了但没建实例」留下半截数据，也会让后续探测把空 keyring 当证据。
func TestSaveProfileLeavesNoKeyringState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("AC_BASE", filepath.Join(home, "appclone"))
	e, err := core.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	p := core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/bin/true",
		Paths: []string{"~/.config/demo"}, ProbeMethod: "overlay",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
		IsolateKeyring: true, KeyringSet: true,
	}
	if err := core.ValidateProfile(p, e.BuildOptions()); err != nil {
		t.Fatalf("档案应通过校验：%v", err)
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	got, err := e.LoadProfile("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsolateKeyring || !got.KeyringSet {
		t.Fatalf("显式开启必须原样回读：%+v", got)
	}
	err = filepath.WalkDir(e.Base, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.Name() == "keyring" || d.Name() == "keyring.pw" {
			t.Fatalf("保存档案不得创建 keyring 状态：%s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 「独立密钥环」行写回档案时必须是显式三态（KeyringSet），且开启后
// DBus/runtime 行被点亮并锁住——界面上看到的隐含关系与 bwrap 实际行为一致。
func TestBuildProfileKeyringExplicitAndRowSync(t *testing.T) {
	if os.Getenv("APPCLONE_GTK_TEST") != "1" {
		t.Skip("GTK 回归需要显式开关：design/guitest-gtk.sh")
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("没有可用的显示（DISPLAY / WAYLAND_DISPLAY）")
	}
	gtk.Init()
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
	u := &ui{env: e, lang: "zh_CN"}
	cfg := filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "demo")
	v := &setupView{
		u: u, appKey: "demo",
		profile: core.Profile{App: "demo", Exec: "/bin/true", ProbeMethod: "overlay"},
		cands: []core.Candidate{{
			Path: cfg, Class: core.ClassConfig, DisplayClass: core.ClassConfig,
			Checked: true,
		}},
	}
	v.keyringRow = adw.NewSwitchRow()
	defer v.keyringRow.Unref()
	v.dbusRow = adw.NewSwitchRow()
	defer v.dbusRow.Unref()
	v.runtimeRow = adw.NewSwitchRow()
	defer v.runtimeRow.Unref()

	v.keyringRow.SetActive(true)
	v.syncKeyringRows()
	if !v.dbusRow.GetActive() || !v.runtimeRow.GetActive() {
		t.Fatal("开启独立密钥环必须隐含私有总线与 runtime")
	}
	if v.dbusRow.GetSensitive() || v.runtimeRow.GetSensitive() {
		t.Fatal("隐含开启的行应锁定，避免视觉与行为不一致")
	}
	p := v.buildProfile()
	if !p.IsolateKeyring || !p.KeyringSet {
		t.Fatalf("开启必须写成显式 true：%+v", p)
	}

	v.keyringRow.SetActive(false)
	v.syncKeyringRows()
	if !v.dbusRow.GetSensitive() || !v.runtimeRow.GetSensitive() {
		t.Fatal("关闭后应恢复可操作")
	}
	p = v.buildProfile()
	if p.IsolateKeyring || !p.KeyringSet {
		t.Fatalf("关闭必须写成显式 false（不是「未设置」）：%+v", p)
	}
}
