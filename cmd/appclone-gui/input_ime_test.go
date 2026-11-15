//go:build linux && gtk_e2e

package main

// 真实中文输入法用例（Wayland 原生，显式 opt-in）。
//
// 不能自动切输入法：IME 引擎与激活态属于用户桌面配置，用例既不读取也不改写。
// 调用者先手动切到中文（例如 fcitx5 的 pinyin），再同时给出
// APPCLONE_GTK_UINPUT=1 与 APPCLONE_GTK_IME=1；否则用例 SKIP。门禁按这一约定
// 校验 PASS/SKIP，不会把「跳过」当成通过。
//
// 覆盖的是输入法提交语义而不是产品回调：uinput 虚拟键盘把拼音键送进宿主 seat，
// 事件经 libinput → GDK → GtkSearchEntry → 输入法，空格提交候选（中文），再用
// 退格按提交后的字符数逐个删除。断言搜索框确实收到 CJK 提交、产品 query 已由
// search-changed 防抖回调归一化、列表按该 query 过滤，以及退格后列表恢复。
// 不依赖具体候选词：只要提交结果里出现 CJK 就通过。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gdk"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"
)

const (
	keyN     = 49
	keyI     = 23
	keyH     = 35
	keyA     = 30
	keySpace = 57
)

func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func TestGtkRealInputChineseIME(t *testing.T) {
	skipUnlessUinput(t)
	if os.Getenv("APPCLONE_GTK_IME") != "1" {
		t.Skip("真实中文输入法用例需要显式 APPCLONE_GTK_IME=1：先把桌面输入法切到中文")
	}
	env, appsDir := gtkTestEnv(t, "uinputime")
	for name, body := range map[string]string{
		"demo.desktop":  "[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n",
		"alpha.desktop": "[Desktop Entry]\nType=Application\nName=Alpha\nExec=/usr/bin/true\n",
	} {
		if err := os.WriteFile(filepath.Join(appsDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	touch, err := newTouchscreen()
	if err != nil {
		t.Fatalf("创建虚拟触摸屏失败（%v）；真实注入用例无法运行", err)
	}
	defer touch.Close()
	kbd, err := newKeyboard([]uint16{keyN, keyI, keyH, keyA, keyO, keySpace, keyBackspace})
	if err != nil {
		t.Fatalf("创建虚拟键盘失败：%v", err)
	}
	defer kbd.Close()

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkUinputIME",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	u := &ui{app: app, env: env, lang: "zh_CN"}
	var runErr error
	activated := false
	focusAbort := ""
	fail := func(format string, a ...any) {
		if runErr == nil {
			runErr = fmt.Errorf(format, a...)
		}
		app.Quit()
	}

	step := 0
	stepDeadline := time.Time{}
	var pending []uint16
	calibrated := false
	calibratedAt := time.Time{}
	lastCalibTap := time.Time{}
	calibPressed := false
	calibTaps := 0
	var calibX, calibY float64
	fullscreenAt := time.Time{}
	envReadyAt := time.Time{}
	var stableX, stableY float64
	stableTicks := 0
	layoutAt := time.Time{}
	var dX, dY, dW, dH int32
	var monX, monY int32
	var committed string

	injectOK := func() bool {
		if u.win != nil && u.win.IsActive() && u.win.IsFullscreen() {
			if ok, name := gdkBackendIsWayland(); !ok {
				focusAbort = fmt.Sprintf("GDK 后端不是 Wayland（%q），未注入任何事件", name)
				app.Quit()
				return false
			}
			return true
		}
		focusAbort = "窗口失去焦点或退回非全屏，已安全中止注入"
		app.Quit()
		return false
	}

	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		activated = true
		u.buildWindow()
		u.win.Present()
		deadline := time.Now().Add(60 * time.Second)
		fullscreenRequested := false

		gesture := gtk.NewGestureClick()
		gesture.SetPropagationPhase(gtk.PhaseCaptureValue)
		var calibCb func(gtk.GestureClick, int32, float64, float64)
		calibCb = func(_ gtk.GestureClick, _ int32, x, y float64) {
			calibPressed = true
			calibX, calibY = x, y
		}
		gesture.ConnectPressed(&calibCb)
		u.win.AddController(&gesture.EventController)
		u.hold(&calibCb, gesture)

		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				fail("真实中文输入法用例超时：卡在第 %d 步（已提交 %q）", step, committed)
				return false
			}
			if step > 0 && !stepDeadline.IsZero() && time.Now().After(stepDeadline) {
				fail("第 %d 步没有等到预期状态（输入法未提交？已提交 %q）", step, committed)
				return false
			}
			if !u.loaded || !u.listSettled() || !u.envChecked || !u.stack.GetMapped() || !u.win.GetMapped() {
				return true
			}
			if !fullscreenRequested {
				fullscreenAt = time.Now()
				u.win.FullscreenOnMonitor(monitorAtOrigin())
				fullscreenRequested = true
				return true
			}
			if !u.win.IsActive() {
				if time.Since(fullscreenAt) > 6*time.Second {
					focusAbort = "窗口在 6 秒内没有获得焦点，未注入任何事件"
					app.Quit()
					return false
				}
				u.win.Present()
				return true
			}
			if envReadyAt.IsZero() {
				envReadyAt = time.Now()
			}
			if time.Since(envReadyAt) < 400*time.Millisecond {
				return true
			}

			mon := windowMonitor(u)
			if mon == nil {
				return true
			}
			var geo gdk.Rectangle
			mon.GetGeometry(&geo)
			if u.win.GetWidth() < geo.Width-2 {
				return true
			}
			if layoutAt.IsZero() {
				layoutAt = time.Now()
			}
			if time.Since(layoutAt) < 300*time.Millisecond {
				return true
			}
			monX, monY = geo.X, geo.Y
			if dW == 0 {
				var ok bool
				dX, dY, dW, dH, ok = desktopBounds()
				if !ok || dW <= 0 || dH <= 0 {
					fail("拿不到桌面尺寸，无法换算触摸坐标")
					return false
				}
			}

			// 搜索框位置连续两拍不动才注入，避开视图切换动画。
			sx, sy, ok := widgetCenter(&u.search.Widget, &u.win.Widget)
			if !ok {
				return true
			}
			if sx != stableX || sy != stableY {
				stableX, stableY = sx, sy
				stableTicks = 0
				return true
			}
			stableTicks++
			if stableTicks < 2 {
				return true
			}

			if !calibrated {
				if !u.win.IsFullscreen() {
					return true
				}
				if ok, name := gdkBackendIsWayland(); !ok {
					focusAbort = fmt.Sprintf("GDK 后端不是 Wayland（%q），未注入任何事件", name)
					app.Quit()
					return false
				}
				if calibratedAt.IsZero() {
					calibratedAt = time.Now()
					lastCalibTap = calibratedAt
					touch.tapAt(float64(monX)+60, float64(monY)+60, dX, dY, dW, dH)
					calibTaps++
					return true
				}
				if calibPressed && calibX > 20 && calibX < 140 && calibY > 20 && calibY < 140 {
					calibrated = true
					u.win.RemoveController(&gesture.EventController)
					return true
				}
				// 合成触摸可能偶尔丢失：有界补点，避免一次丢点就直接 SKIP，
				// 让 opt-in 门禁误报成「找不到 PASS」。以最后一次点击计时，
				// 补点之间至少隔 1s；总中止仍从第一次点击算 4s。
				if time.Since(lastCalibTap) > 1*time.Second && calibTaps < 3 {
					lastCalibTap = time.Now()
					touch.tapAt(float64(monX)+60, float64(monY)+60, dX, dY, dW, dH)
					calibTaps++
					return true
				}
				if time.Since(calibratedAt) > 4*time.Second {
					focusAbort = "触摸校准点击没有回到本窗口（输入映射不是整块桌面，或窗口未真正全屏），未继续注入"
					app.Quit()
					return false
				}
				return true
			}

			switch step {
			case 0:
				if !injectOK() {
					return false
				}
				touch.tapAt(float64(monX)+sx, float64(monY)+sy, dX, dY, dW, dH)
				step = 1
				stepDeadline = time.Now().Add(4 * time.Second)
			case 1:
				if !focusWithin(u, &u.search.Widget) {
					return true
				}
				if !injectOK() {
					return false
				}
				// 先只发五个拼音键；空格留到下一阶段，确认预编辑还没提交。
				pending = []uint16{keyN, keyI, keyH, keyA, keyO}
				step = 2
				stepDeadline = time.Now().Add(6 * time.Second)
			case 2, 3, 5:
				// 每 tick 只发一个键：失焦能在按键间被主循环看到，不会把
				// 剩余按键连发进别的窗口。
				if !injectOK() {
					return false
				}
				if !focusWithin(u, &u.search.Widget) {
					focusAbort = "搜索框失去焦点，已安全中止注入"
					app.Quit()
					return false
				}
				if len(pending) > 0 {
					kbd.pressKey(pending[0])
					pending = pending[1:]
					stepDeadline = time.Now().Add(4 * time.Second)
					return true
				}
				if step == 2 {
					// 预编辑阶段（拼音已输入、候选未提交）不得出现汉字；否则
					// 后面的 CJK 断言就无法证明空格提交真的生效。预编辑文本
					// 是否可见随输入法实现不同，这里只钉住「尚未提交」。
					if hasCJK(u.search.GetText()) {
						fail("空格提交前搜索框已出现 CJK（预编辑被提前提交？）：%q", u.search.GetText())
						return false
					}
					pending = []uint16{keySpace}
					step = 3
					stepDeadline = time.Now().Add(4 * time.Second)
					return true
				}
				if step == 3 {
					step = 4
					stepDeadline = time.Now().Add(6 * time.Second)
					return true
				}
				// 退格已全部发出。
				step = 6
				stepDeadline = time.Now().Add(6 * time.Second)
				return true
			case 4:
				// 空格提交后等文本落地；输入法把候选提交到 GtkSearchEntry。
				text := u.search.GetText()
				if !hasCJK(text) {
					return true
				}
				if u.query != strings.ToLower(strings.TrimSpace(text)) {
					return true
				}
				// 产品的过滤必须真的按提交后的中文 query 跑过：Alpha 不含这些
				// 汉字，应该已被过滤掉。
				if row := findListRow(u.groups[0], "Alpha"); row != nil {
					row.Unref()
					fail("中文查询 %q 后 Alpha 行仍在列表中（过滤没有接线？）", text)
					return false
				}
				committed = text
				n := len([]rune(text))
				if n == 0 {
					return true
				}
				if !injectOK() {
					return false
				}
				pending = make([]uint16, n)
				for i := range pending {
					pending[i] = keyBackspace
				}
				step = 5
				stepDeadline = time.Now().Add(6 * time.Second)
			case 6:
				if u.search.GetText() != "" || u.query != "" {
					return true
				}
				row := findListRow(u.groups[0], "Alpha")
				if row == nil {
					return true
				}
				row.Unref()
				u.win.Unfullscreen()
				// 完成标志只在恢复验证通过后置位：若外部关窗让主循环在
				// 这里之前退出，step 仍是 6，终态断言必须失败而不是假绿。
				step = 7
				app.Quit()
				return false
			}
			return true
		}
		glib.TimeoutAdd(50, &tick, 0)
	}
	app.ConnectActivate(&actcb)
	u.hold(&actcb)

	var watchdog glib.SourceFunc
	watchdog = func(uintptr) bool {
		if !activated {
			fail("GTK activate 未在 60 秒内触发（没有可用显示？）")
			app.Quit()
		}
		return false
	}
	glib.TimeoutAdd(60000, &watchdog, 0)
	u.hold(&watchdog)

	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK 主循环退出码 %d", code)
	}
	if focusAbort != "" {
		t.Skip(focusAbort)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !activated || step != 7 || !hasCJK(committed) {
		t.Fatalf("真实中文输入法用例没有跑完：activated=%t step=%d committed=%q", activated, step, committed)
	}
	t.Logf("中文输入法提交/退格完成：committed=%q runes=%d", committed, len([]rune(committed)))
}
