//go:build linux && gtk_e2e

package main

// 旧档案「就地重新探测」的 GTK 回归。
//
// 覆盖四件事：
//   - 横幅按钮只在「open + 读证据不完整 + FUSE 可用 + 无探测在飞」时出现，
//     空标签会把 banner 内部按钮真正隐藏（不是留一个空按钮）；
//   - 取消重探 = 什么都没发生：页面不 pop、路径勾选与非路径开关原样恢复，
//     旧探测的迟到结果/失败按 epoch 丢弃（drain 期间不碰已恢复的控件）；
//   - 重探落案：非路径字段保留、旧路径不写回、范围变化逐条可见、降级横幅
//     跟着新档案走、确认前不写盘；
//   - error 态保留快照：取消能回到确认页，reader_stuck 不挂重试。
//
// 闸门里**不启动真实探测**（探测要真启动目标应用，属于冒烟范围）：真实链路
// 由手动用例 TestGtkLegacyReprobeRealProbe 覆盖，这里用 beginLegacyReprobe()
// 与 handleProbeFailure/handleProbeDone 这些与生产同一条的处理函数驱动状态机。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

const legacyReprobeApp = "demo"

// legacyReprobeFixture 建一个「旧版本生成的档案」：路径可用、可复用，但
// read_trace 缺失、探测方式是 strace。open 入口因此直接进确认页并带旧档案横幅。
func legacyReprobeFixture(t *testing.T, name string) (*core.Env, *ui, *adw.Application) {
	t.Helper()
	env, appsDir := gtkTestEnv(t, name)
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 登录状态行要能拨动：能力检测走真实 PATH 会让断言随机器漂移。
	// 自检报告 FUSE 可用，否则重探入口按设计隐藏（noFuseMounter 会让页面
	// 进入降级态，用例就测不到按钮）。这里从不真正挂载：闸门不起真探测。
	env.Mounter = availableMounter{}
	env.KeyringCapsOverride = &core.KeyringCaps{
		Available: true, Daemon: "/usr/bin/gnome-keyring-daemon", DbusSend: "/usr/bin/dbus-send",
	}
	if err := env.SaveProfile(core.Profile{
		App: legacyReprobeApp, Version: core.ProfileVersion, Exec: "/usr/bin/true",
		ProbeMethod: "strace", ReadTrace: "",
		Paths:       []string{"~/.config/Demo", "~/.local/share/Demo"},
		IsolateDBus: true,
		Env:         map[string]string{}, Ignored: core.DefaultIgnored(),
	}); err != nil {
		t.Fatal(err)
	}
	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkLegacyReprobe"+name,
		gio.GApplicationNonUniqueValue)
	u := &ui{app: app, env: env, lang: "zh_CN"}
	return env, u, app
}

func runLegacyReprobeApp(t *testing.T, u *ui, app *adw.Application, body func(*setupView) (bool, error)) {
	t.Helper()
	var runErr error
	step := 0
	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		u.buildWindow()
		deadline := time.Now().Add(25 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				state := "<nil>"
				if v := u.activeSetup; v != nil {
					state = v.state
				}
				if runErr == nil {
					runErr = fmt.Errorf("旧档案重探用例超时（step=%d state=%s）", step, state)
				}
				app.Quit()
				return false
			}
			switch step {
			case 0:
				if !u.loaded || !u.listSettled() {
					return true
				}
				if !u.pushSetup(legacyReprobeApp, "open", func(bool) {}) {
					runErr = errors.New("旧档案的确认页没有推出来")
					app.Quit()
					return false
				}
				step = 1
			case 1:
				v := u.activeSetup
				if v == nil || v.state != "confirm" {
					return true
				}
				done, err := body(v)
				if err != nil {
					runErr = err
					app.Quit()
					return false
				}
				if done {
					app.Quit()
					return false
				}
			}
			return true
		}
		glib.TimeoutAdd(20, &tick, 0)
	}
	u.hold(&actcb)
	app.ConnectActivate(&actcb)
	code := app.Run(1, os.Args[:1])
	// 失败路径可能停在 startSimulatedProbe 的 Hold 上：不释放会让 GApplication
	// 一直持有 use count，测试只能等 go test 超时。
	if v := u.activeSetup; v != nil {
		v.release()
	}
	if code != 0 {
		t.Fatalf("GTK 主循环退出码 %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
}

// bannerButtonVisible 沿 AdwBanner 的内部子控件找到 button，返回其可见性。
// libadwaita 的公开 API 没有"隐藏按钮"的开关；产品靠空标签让 banner 自己隐藏
// 按钮，这条断言把那个行为钉住（换成空按钮就是视觉回归）。
func bannerButtonVisible(b *adw.Banner) (visible, found bool) {
	var hit *gtk.Widget
	var walk func(w *gtk.Widget)
	walk = func(w *gtk.Widget) {
		if hit != nil {
			return
		}
		if w.GetCssName() == "button" {
			hit = w
			return
		}
		for c := w.GetFirstChild(); c != nil; c = c.GetNextSibling() {
			walk(c)
		}
	}
	walk(&b.Widget)
	if hit == nil {
		return false, false
	}
	return hit.GetVisible(), true
}

// startSimulatedProbe 把页面推进到「一场就地重探正在跑」的状态，等价于
// probe() 开头那段：领 epoch、Hold、探测中。不启动 core.Probe。
func startSimulatedProbe(v *setupView) uint64 {
	v.probeEpoch++
	v.probeInFlight = true
	v.probeControl = &core.ProbeControl{}
	v.hold()
	v.setState("probing")
	return v.probeEpoch
}

func TestGtkLegacyReprobeBanner(t *testing.T) {
	env, u, app := legacyReprobeFixture(t, "Banner")
	_ = env
	runLegacyReprobeApp(t, u, app, func(v *setupView) (bool, error) {
		action := u.tr("ui.setup.reads.reprobe.action", nil)
		if !v.banner.GetRevealed() || !strings.Contains(v.banner.GetTitle(), u.tr("ui.setup.reads.legacy", nil)) {
			return false, fmt.Errorf("旧档案确认页缺少读证据横幅：%q", v.banner.GetTitle())
		}
		if got := v.banner.GetButtonLabel(); got != action {
			return false, fmt.Errorf("旧档案横幅缺少「重新探测」按钮：%q", got)
		}
		if visible, found := bannerButtonVisible(v.banner); !found || !visible {
			return false, fmt.Errorf("横幅按钮应可见（found=%v visible=%v）", found, visible)
		}

		// 探测中：按钮消失；没有告警时整条横幅也隐藏。
		v.degraded = false
		v.setState("probing")
		if got := v.banner.GetButtonLabel(); got != "" {
			return false, fmt.Errorf("探测中横幅按钮必须隐藏，实际 %q", got)
		}
		if visible, _ := bannerButtonVisible(v.banner); visible {
			return false, errors.New("空标签没有隐藏 banner 内部按钮（会留一个空按钮）")
		}
		if v.banner.GetRevealed() {
			return false, errors.New("没有告警时横幅应隐藏")
		}
		v.setState("confirm")

		// 只有 open + 读证据不完整 + FUSE 可用时才给入口。
		v.readTrace = core.ReadTraceOK
		v.syncReadsBanner()
		if got := v.banner.GetButtonLabel(); got != "" {
			return false, fmt.Errorf("读证据完整时不该出现重探按钮：%q", got)
		}
		// partial/unavailable 同样是「读证据不完整」，入口不能只给空 read_trace。
		for _, rt := range []string{core.ReadTracePartial, core.ReadTraceUnavailable} {
			v.readTrace = rt
			v.syncReadsBanner()
			if got := v.banner.GetButtonLabel(); got != action {
				return false, fmt.Errorf("read_trace=%q 时缺少重探按钮：%q", rt, got)
			}
		}
		v.readTrace = ""
		v.syncReadsBanner()
		// bwrap 缺失（致命态）时探测必然在闸门失败，按钮不能装作可用。
		v.u.fatal = true
		v.syncReadsBanner()
		if got := v.banner.GetButtonLabel(); got != "" {
			return false, fmt.Errorf("致命态下不该出现重探按钮：%q", got)
		}
		v.u.fatal = false
		v.syncReadsBanner()
		v.mode = "clone"
		v.syncReadsBanner()
		if got := v.banner.GetButtonLabel(); got != "" {
			return false, fmt.Errorf("clone 入口不该出现重探按钮：%q", got)
		}
		v.mode = "open"
		v.u.fuseBlocked = true
		v.syncReadsBanner()
		if got := v.banner.GetButtonLabel(); got != "" {
			return false, fmt.Errorf("FUSE 不可用时不该出现重探按钮：%q", got)
		}
		v.u.fuseBlocked = false

		// drain 期间（旧探测还在回收）按钮消失，而且此时触发的信号不能
		// 留下快照——否则 drain 结束后按钮会永久消失、确认页基线错位。
		v.reprobeDraining = true
		v.syncReadsBanner()
		if got := v.banner.GetButtonLabel(); got != "" {
			return false, fmt.Errorf("drain 期间不该出现重探按钮：%q", got)
		}
		gobject.SignalEmitByName(&v.banner.Widget.Object, "button-clicked")
		if v.reprobeSnap != nil || v.reprobeFromConfirm || v.state != "confirm" {
			return false, errors.New("drain 期间横幅信号仍启动了重探")
		}
		v.reprobeDraining = false
		v.syncReadsBanner()
		if got := v.banner.GetButtonLabel(); got != action {
			return false, fmt.Errorf("drain 结束后按钮应恢复：%q", got)
		}

		// 正向接线：真实点横幅按钮必须走到 probe()。probeHook 让 probe() 停在
		// 进入探测态的那一刻，不在 CI 里启动目标应用（真实链路由手动用例覆盖）。
		wired := false
		v.probeHook = func() { wired = true }
		gobject.SignalEmitByName(&v.banner.Widget.Object, "button-clicked")
		if !wired || v.reprobeSnap == nil || !v.reprobeFromConfirm || v.state != "probing" {
			return false, fmt.Errorf("横幅按钮没有接到重探：wired=%v snap=%v flag=%v state=%s",
				wired, v.reprobeSnap != nil, v.reprobeFromConfirm, v.state)
		}
		if !v.cancelReprobe() {
			return false, errors.New("重探中取消没有恢复确认页")
		}
		v.settleProbe()
		if v.held || v.probeInFlight || v.reprobeDraining {
			return false, errors.New("接线用例结束后 Hold/探测状态没有收干净")
		}
		v.probeHook = nil
		return true, nil
	})
}

func TestGtkLegacyReprobeCancel(t *testing.T) {
	env, u, app := legacyReprobeFixture(t, "Cancel")
	_ = env
	runLegacyReprobeApp(t, u, app, func(v *setupView) (bool, error) {
		action := u.tr("ui.setup.reads.reprobe.action", nil)
		if len(v.rows) < 2 {
			return false, fmt.Errorf("旧档案应有 2 条可勾选路径，实际 %d", len(v.rows))
		}
		// 用户先改过确认页：取消重探必须把这些改动原样带回来。
		v.rows[0].SetActive(true)
		v.rows[1].SetActive(false)
		v.dbusRow.SetActive(false)
		v.forkRow.SetActive(true)
		if !v.beginLegacyReprobe() {
			return false, errors.New("旧档案确认页不能开始重探")
		}
		if v.reprobeSnap == nil || !v.reprobeFromConfirm {
			return false, errors.New("重探开始前没有保存快照/置 flag")
		}
		epoch := startSimulatedProbe(v)
		if v.page.GetCanPop() {
			return false, errors.New("重探中返回手势必须被挡住")
		}

		// 取消按钮 = 取消这场探测并回到确认页，不是关闭整个开启流程。
		gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
		if v.done {
			return false, errors.New("取消重探不应结束页面")
		}
		if v.state != "confirm" || v.reprobeSnap != nil || v.reprobeFromConfirm {
			return false, fmt.Errorf("取消后没有回到原确认页：state=%s snap=%v flag=%v",
				v.state, v.reprobeSnap != nil, v.reprobeFromConfirm)
		}
		if !v.page.GetCanPop() {
			return false, errors.New("恢复确认页后返回手势应恢复")
		}
		if !v.reprobeDraining {
			return false, errors.New("取消后旧探测未回收，必须进入 drain")
		}
		if !v.rows[0].GetActive() || v.rows[1].GetActive() || v.dbusRow.GetActive() || !v.forkRow.GetActive() {
			return false, errors.New("取消重探没有恢复用户改过的勾选/开关")
		}
		if got := v.banner.GetButtonLabel(); got != "" {
			return false, fmt.Errorf("drain 期间重探按钮必须隐藏：%q", got)
		}
		if v.confirmBtn.GetSensitive() {
			return false, errors.New("drain 期间确认按钮必须置灰（旧探测还持写锁）")
		}

		// drain 期间旧探测的迟到结果/失败一律丢弃。
		v.handleProbeFailure(epoch, errors.New("late failure"))
		if v.state != "confirm" || v.readTrace != "" {
			return false, fmt.Errorf("迟到失败把恢复后的确认页覆盖成了错误页：state=%s", v.state)
		}
		v.handleProbeDone(epoch, nil, core.ProbeResult{}, core.Profile{}, nil, 0)
		if v.state != "confirm" || v.readTrace != "" {
			return false, errors.New("迟到结果改写了恢复后的确认页")
		}

		v.settleProbe()
		if v.reprobeDraining || v.probeInFlight || v.held {
			return false, errors.New("settleProbe 没有清掉 drain/探测/Hold")
		}
		if got := v.banner.GetButtonLabel(); got != action {
			return false, fmt.Errorf("drain 结束后重探按钮应恢复：%q", got)
		}
		if !v.confirmBtn.GetSensitive() {
			return false, errors.New("drain 结束后确认按钮应恢复可用")
		}

		// 第一次取消后再次开始：基线必须重新取。快照若用 append([]string{}, nil...)
		// 恢复成"非 nil 空切片"，nil 哨兵失效，第二次重探就再也不会计算基线，
		// 范围收窄会在无告警的情况下落盘（P1 回归）。
		if v.reprobeBaseline != nil {
			return false, fmt.Errorf("取消后基线应回到未取的 nil，实际 %v", v.reprobeBaseline)
		}
		if !v.beginLegacyReprobe() {
			return false, errors.New("第一次取消后不能再开始重探")
		}
		if len(v.reprobeBaseline) != 2 {
			return false, fmt.Errorf("第二次重探没有重新取基线：%v", v.reprobeBaseline)
		}
		startSimulatedProbe(v)
		gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
		if v.state != "confirm" || v.reprobeBaseline != nil {
			return false, fmt.Errorf("第二次取消后基线应回到 nil：state=%s baseline=%v",
				v.state, v.reprobeBaseline)
		}
		v.settleProbe()

		// 第二次重探（第一次落案后读证据仍不完整）：取消必须带回原确认页的
		// 范围变化告警与原始基线，不能在无提示下确认收窄后的档案（P1 回归）。
		v.readTrace = core.ReadTracePartial
		v.reprobeFromConfirm = true
		v.reprobeBaseline = []string{"~/.config/Demo", "~/.local/share/Demo", "~/.local/share/Demo-old"}
		v.reprobeDropped = nil
		v.syncReadsBanner()
		if !v.beginLegacyReprobe() {
			return false, errors.New("第二次重探不能从落案后的确认页开始")
		}
		if v.reprobeSnap == nil || !v.reprobeSnap.reprobeFromConfirm {
			return false, errors.New("第二次重探的快照没有带上重探状态")
		}
		startSimulatedProbe(v)
		gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
		if v.state != "confirm" || !v.reprobeFromConfirm || v.reprobeSnap != nil {
			return false, fmt.Errorf("第二次取消没有回到带告警的确认页：state=%s flag=%v snap=%v",
				v.state, v.reprobeFromConfirm, v.reprobeSnap != nil)
		}
		wantDropped := "~/.local/share/Demo、~/.local/share/Demo-old"
		if got := strings.Join(v.reprobeDropped, "、"); got != wantDropped {
			return false, fmt.Errorf("第二次取消后范围变化告警丢了：%q（want %q）", got, wantDropped)
		}
		wantMsg := u.tr("ui.setup.reads.reprobe_dropped", map[string]any{
			"count": 2, "paths": []string{"~/.local/share/Demo", "~/.local/share/Demo-old"},
		})
		if !strings.Contains(v.banner.GetTitle(), wantMsg) {
			return false, fmt.Errorf("恢复后的横幅缺少范围变化说明：%q", v.banner.GetTitle())
		}
		v.settleProbe()

		// 完成先到 + 取消：确认页上的取消就是普通取消（finish(false)）。
		gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
		if !v.done || v.success {
			return false, errors.New("确认页取消没有按 finish(false) 处理")
		}
		return true, nil
	})
}

func TestGtkLegacyReprobeLanded(t *testing.T) {
	env, u, app := legacyReprobeFixture(t, "Landed")
	runLegacyReprobeApp(t, u, app, func(v *setupView) (bool, error) {
		v.rows[1].SetActive(false)
		v.keyringRow.SetActive(true)
		// syncKeyringRows 会顺手点亮 DBus 行；这里显式关掉，用来验证合并时
		// 以确认页上的显式选择为准（探测产物里没有非路径字段）。
		v.dbusRow.SetActive(false)
		if !v.beginLegacyReprobe() {
			return false, errors.New("旧档案确认页不能开始重探")
		}
		epoch := startSimulatedProbe(v)

		cfg := filepath.Join(env.Resolver.Home, ".config", "Demo")
		res := core.ProbeResult{
			Candidates: []core.Candidate{{Path: cfg, Class: core.ClassConfig, Checked: true, Why: "probe.why.config"}},
			ReadTrace:  core.ReadTraceOK,
			// 探测产物里没有非路径字段：合并必须保留确认页上的显式选择。
			Profile: core.Profile{App: legacyReprobeApp, ProbeMethod: "overlay", IsolateDBus: false},
		}
		p, cands, prefilled, err := env.FinalizeProbe(legacyReprobeApp, res, core.ProbeFinalize{
			Existing: v.existing, HasExisting: true,
		})
		if err != nil {
			return false, fmt.Errorf("合成重探结果落案失败：%v", err)
		}
		v.handleProbeDone(epoch, nil, res, p, cands, prefilled)

		if v.state != "confirm" || v.reprobeSnap != nil || !v.reprobeFromConfirm {
			return false, fmt.Errorf("重探结果没有落成确认页：state=%s snap=%v flag=%v",
				v.state, v.reprobeSnap != nil, v.reprobeFromConfirm)
		}
		if v.readTrace != core.ReadTraceOK || v.degraded {
			return false, fmt.Errorf("新档案的读证据/降级横幅没有跟着更新：trace=%q degraded=%v",
				v.readTrace, v.degraded)
		}
		if len(v.profile.Paths) != 1 || v.profile.Paths[0] != "~/.config/Demo" {
			return false, fmt.Errorf("paths 应按本次候选重新定案：%v", v.profile.Paths)
		}
		if !v.profile.IsolateKeyring || v.profile.IsolateDBus {
			return false, fmt.Errorf("非路径字段没有保留确认页上的选择：keyring=%v dbus=%v",
				v.profile.IsolateKeyring, v.profile.IsolateDBus)
		}
		wantDropped := "~/.local/share/Demo"
		if len(v.reprobeDropped) != 1 || v.reprobeDropped[0] != wantDropped {
			return false, fmt.Errorf("范围变化没有逐条报出：%v", v.reprobeDropped)
		}
		wantMsg := u.tr("ui.setup.reads.reprobe_dropped", map[string]any{"count": 1, "paths": wantDropped})
		if !strings.Contains(v.banner.GetTitle(), wantMsg) {
			return false, fmt.Errorf("横幅缺少范围变化说明（want %q, got %q）", wantMsg, v.banner.GetTitle())
		}
		if got := v.banner.GetButtonLabel(); got != "" {
			return false, fmt.Errorf("读证据完整后不该再有重探按钮：%q", got)
		}
		// 用户再拨勾选：范围变化必须重算，不能承诺与实际落盘相反的结果。
		v.rows[0].SetActive(false)
		if len(v.reprobeDropped) != 2 {
			return false, fmt.Errorf("取消勾选后应报出两条不再隔离的路径：%v", v.reprobeDropped)
		}
		v.rows[0].SetActive(true)

		// 确认前不写盘、不建实例。
		if disk, err := env.LoadProfile(legacyReprobeApp); err != nil || disk.ReadTrace != "" {
			return false, fmt.Errorf("确认前档案被改写了：err=%v read_trace=%q", err, disk.ReadTrace)
		}
		if entries, err := os.ReadDir(filepath.Join(env.Base, "instances", legacyReprobeApp)); err == nil && len(entries) > 0 {
			return false, errors.New("确认前不应创建实例目录")
		}

		// 完成先到：此时点取消按确认页取消处理。
		gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
		if !v.done || v.success {
			return false, errors.New("落案后的取消没有按 finish(false) 处理")
		}
		return true, nil
	})
}

func TestGtkLegacyReprobeErrorPaths(t *testing.T) {
	env, u, app := legacyReprobeFixture(t, "ErrorPaths")
	_ = env
	runLegacyReprobeApp(t, u, app, func(v *setupView) (bool, error) {
		action := u.tr("ui.setup.reads.reprobe.action", nil)
		reset := func() error {
			v.rows[0].SetActive(true)
			v.rows[1].SetActive(false)
			v.forkRow.SetActive(true)
			return nil
		}
		if err := reset(); err != nil {
			return false, err
		}

		// 1) 重探失败 → 取消 → 回到确认页（不是关掉流程）。
		if !v.beginLegacyReprobe() {
			return false, errors.New("失败用例不能开始重探")
		}
		epoch := startSimulatedProbe(v)
		v.handleProbeFailure(epoch, errors.New("boom"))
		if v.state != "error" || v.retry == nil {
			return false, fmt.Errorf("普通失败应进 error 且可重试：state=%s retry=%v", v.state, v.retry != nil)
		}
		if v.page.GetCanPop() {
			return false, errors.New("error 态有快照时返回手势必须被挡住")
		}
		gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
		if v.state != "confirm" || v.readTrace != "" || v.reprobeDraining {
			return false, fmt.Errorf("error 取消没有恢复确认页：state=%s trace=%q drain=%v",
				v.state, v.readTrace, v.reprobeDraining)
		}
		if !v.rows[0].GetActive() || v.rows[1].GetActive() || !v.forkRow.GetActive() {
			return false, errors.New("error 取消没有恢复行勾选/开关")
		}
		if got := v.banner.GetButtonLabel(); got != action {
			return false, fmt.Errorf("恢复后重探按钮应回来：%q", got)
		}

		// 2) reader_stuck：进程级不可恢复，不挂重试，取消仍能恢复。
		if !v.beginLegacyReprobe() {
			return false, errors.New("reader_stuck 用例不能开始重探")
		}
		epoch = startSimulatedProbe(v)
		v.handleProbeFailure(epoch, &core.Error{Code: core.CodeEnv, Key: "err.probe.reader_stuck"})
		if v.state != "error" || v.retry != nil {
			return false, fmt.Errorf("reader_stuck 不能挂重试：state=%s retry=%v", v.state, v.retry != nil)
		}
		if v.confirmBtn.GetVisible() {
			return false, errors.New("reader_stuck 错误页不应显示重试按钮")
		}
		gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
		if v.state != "confirm" || v.reprobeSnap != nil || v.reprobeFromConfirm {
			return false, errors.New("reader_stuck 取消没有恢复确认页")
		}

		// 3) error → 重试 → 取消：新 epoch 作废旧失败，取消仍回确认页。
		if !v.beginLegacyReprobe() {
			return false, errors.New("重试用例不能开始重探")
		}
		oldEpoch := startSimulatedProbe(v)
		v.handleProbeFailure(oldEpoch, errors.New("boom"))
		if v.state != "error" {
			return false, fmt.Errorf("重试用例没有进入 error：%s", v.state)
		}
		retryEpoch := startSimulatedProbe(v)
		if retryEpoch == oldEpoch {
			return false, errors.New("重试没有领新 epoch")
		}
		v.handleProbeDone(oldEpoch, nil, core.ProbeResult{}, core.Profile{}, nil, 0)
		if v.state != "probing" {
			return false, errors.New("旧 epoch 的结果改写了重试中的页面")
		}
		gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
		if v.state != "confirm" || v.done {
			return false, fmt.Errorf("重试中取消应回到确认页：state=%s done=%v", v.state, v.done)
		}
		v.settleProbe()
		return true, nil
	})
}
