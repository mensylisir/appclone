//go:build linux && gtk_e2e

package main

// 真实指针/键盘注入用例（Wayland 原生）：不调产品回调、不合成 GTK 事件，
// 而是让内核 uinput 产生的真实触摸/按键事件穿过 libinput → GDK → 控件。
//
// 覆盖 docs/STATUS.md 里此前标注"未覆盖"的两条：
//   - 视图切换器的指针点击（点左/右两个标签，两个方向都验）；
//   - 搜索框的真实键盘输入（点击聚焦 → 逐键输入 → 逐键退格清空 → 列表恢复）。
//     断言不依赖键盘布局：物理键位在不同布局下产生的字符不同，只要求字符确实
//     进了输入框，退格数按实际字符数生成，不写死 `demo`。
//
// 为什么不点行内开关：那会进入真实探测（要执行系统里的应用），属于冒烟脚本
// 的范围；本用例只碰无副作用的控件。窗口会在用例期间全屏，结束后恢复。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gdk"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

// widgetCenter 返回控件中心在 toplevel 坐标系里的位置。
// gdkBackendIsWayland 在真正注入前确认事件目的地：只有 GDK 真的连到 Wayland
// 合成器才继续。环境变量可能被继承（Wayland 桌面里跑 xvfb-run），不可信。
func gdkBackendIsWayland() (bool, string) {
	d := gdk.DisplayGetDefault()
	if d == nil {
		return false, ""
	}
	name := d.GetName()
	return strings.Contains(strings.ToLower(name), "wayland"), name
}

func widgetCenter(w *gtk.Widget, toplevel *gtk.Widget) (float64, float64, bool) {
	if w == nil || w.GetWidth() <= 0 || w.GetHeight() <= 0 {
		return 0, 0, false
	}
	var x, y float64
	if !w.TranslateCoordinates(toplevel, 0, 0, &x, &y) {
		return 0, 0, false
	}
	return x + float64(w.GetWidth())/2, y + float64(w.GetHeight())/2, true
}

// desktopBounds 返回所有显示器几何的并集（逻辑像素）。uinput 触摸屏的绝对
// 坐标覆盖整块桌面，而不是单一显示器——实测由并集决定。
func desktopBounds() (x, y, w, h int32, ok bool) {
	monitors := gdk.DisplayGetDefault().GetMonitors()
	n := monitors.GetNItems()
	if n == 0 {
		return 0, 0, 0, 0, false
	}
	var minX, minY, maxX, maxY int32
	for i := uint32(0); i < n; i++ {
		m := gdk.MonitorNewFromInternalPtr(monitors.GetItem(i))
		if m == nil {
			continue
		}
		var geo gdk.Rectangle
		m.GetGeometry(&geo)
		if !ok {
			minX, minY = geo.X, geo.Y
			maxX, maxY = geo.X+geo.Width, geo.Y+geo.Height
			ok = true
			continue
		}
		if geo.X < minX {
			minX = geo.X
		}
		if geo.Y < minY {
			minY = geo.Y
		}
		if geo.X+geo.Width > maxX {
			maxX = geo.X + geo.Width
		}
		if geo.Y+geo.Height > maxY {
			maxY = geo.Y + geo.Height
		}
	}
	return minX, minY, maxX - minX, maxY - minY, ok
}

// sameWidget 比较底层 GObject 指针：puregotk 每次取父/子都新建包装，直接比
// 包装地址会永远不等。
func sameWidget(a, b *gtk.Widget) bool {
	return a != nil && b != nil && a.GoPointer() == b.GoPointer()
}

// focusWithin 判断窗口当前的焦点控件是不是 w 本身或 w 的后代。
// GtkSearchEntry 是复合控件，焦点落在内部的 GtkText 上，外层 entry 的
// HasFocus() 会是 false——必须沿父链找。
func focusWithin(u *ui, w *gtk.Widget) bool {
	f := u.win.GetFocus()
	for f != nil {
		if sameWidget(f, w) {
			return true
		}
		f = f.GetParent()
	}
	return false
}

// monitorAtOrigin 返回包含桌面原点 (0,0) 的显示器。GNOME/KDE 把主显示器
// 放在原点；全屏固定到它，触摸坐标无论按"整个桌面"还是"单显示器"映射，
// 落点都不会离开这块屏幕——避免把点击送到用户其他显示器上的应用。
func monitorAtOrigin() *gdk.Monitor {
	monitors := gdk.DisplayGetDefault().GetMonitors()
	n := monitors.GetNItems()
	var first *gdk.Monitor
	for i := uint32(0); i < n; i++ {
		m := gdk.MonitorNewFromInternalPtr(monitors.GetItem(i))
		if m == nil {
			continue
		}
		if first == nil {
			first = m
		}
		var geo gdk.Rectangle
		m.GetGeometry(&geo)
		if geo.X <= 0 && geo.Y <= 0 && geo.X+geo.Width > 0 && geo.Y+geo.Height > 0 {
			return m
		}
	}
	return first
}

// windowMonitor 返回窗口当前所在显示器（全屏后就是它铺满的那块）。
func windowMonitor(u *ui) *gdk.Monitor {
	native := u.win.GetNative()
	if native == nil {
		return nil
	}
	surface := native.GetSurface()
	if surface == nil {
		return nil
	}
	return gdk.DisplayGetDefault().GetMonitorAtSurface(surface)
}

// skipUnlessUinput 是一条硬闸门：注入进宿主 seat，只有显式 opt-in、当前是
// Wayland 会话且 /dev/uinput 可写时才继续。X11/Xvfb/嵌套合成器里窗口可以在
// 自己的 server 里 active，事件却落到宿主的其他应用上。
func skipUnlessUinput(t *testing.T) {
	t.Helper()
	if os.Getenv("APPCLONE_GTK_UINPUT") != "1" {
		t.Skip("真实输入注入需要显式 APPCLONE_GTK_UINPUT=1；design/guitest-gtk.sh 会在具备条件时打开")
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("真实输入注入只在 Wayland 会话启用：X11/Xvfb 下事件可能投到宿主应用")
	}
	if f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0); err != nil {
		t.Skipf("打不开 /dev/uinput（%v）；真实注入用例无法运行", err)
	} else {
		_ = f.Close()
	}
}

func TestGtkRealInputUinput(t *testing.T) {
	skipUnlessUinput(t)
	env, appsDir := gtkTestEnv(t, "uinput")
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
	kbd, err := newKeyboard([]uint16{keyD, keyE, keyM, keyO, keyReturn, keyBackspace})
	if err != nil {
		t.Fatalf("创建虚拟键盘失败：%v", err)
	}
	defer kbd.Close()

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkUinput",
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
	calibPressed := false
	var calibX, calibY float64
	fullscreenAt := time.Time{}
	envReadyAt := time.Time{}
	var stableX, stableY float64
	stableTicks := 0
	layoutAt := time.Time{}
	allAt := time.Time{}
	var dX, dY, dW, dH int32
	var monX, monY int32

	// injectOK 在每次注入前复查焦点：窗口不是活动窗口时绝不发事件，
	// 避免把点击/按键送进用户自己的应用。
	injectOK := func() bool {
		if u.win != nil && u.win.IsActive() && u.win.IsFullscreen() {
			// WAYLAND_DISPLAY 可能从宿主继承（例如 Wayland 桌面里跑 xvfb-run），
			// 不能只靠环境变量判定：真正决定事件去哪的是 GDK 后端。X11/Xvfb
			// 下 uinput 事件进宿主 seat，可能落到别的应用上，一律不注入。
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
		deadline := time.Now().Add(40 * time.Second)
		fullscreenRequested := false

		// 校准用：注入前先在窗口上装一个点击观察器，证明触摸事件确实回到
		// 了本窗口（而不是宿主其他显示器/应用）；校准后立刻摘掉，避免干扰
		// 后续控件的正常点击。
		gesture := gtk.NewGestureClick()
		// 捕获相：事件先到观察器，命中搜索框等子控件也不会被它们抢走；
		// 校准这一下因此没有副作用，校准后立即摘掉。
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
				fail("真实注入用例超时：卡在第 %d 步", step)
				return false
			}
			if step > 0 && !stepDeadline.IsZero() && time.Now().After(stepDeadline) {
				fail("第 %d 步没有等到预期状态（注入未生效？）", step)
				return false
			}
			if !u.loaded || !u.envChecked || !u.stack.GetMapped() || !u.win.GetMapped() {
				return true
			}
			if !fullscreenRequested {
				// 从请求全屏开始计时：窗口一直拿不到焦点时，6 秒后必须中止，
				// 不能等 40 秒总超时。
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
			// 自检完成后降级横幅会做 reveal 动画（搜索框、切换器整体下移）。
			// 等动画结束再算坐标，否则点击会落在旧位置。
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
			// 等全屏布局落地：窗口宽度要和显示器宽度一致，再留 300ms 给动画。
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

			// 布局稳定：连续两拍切换器中心不动才注入，避免动画中间点到空白。
			sx, sy, ok := widgetCenter(&u.switcher.Widget, &u.win.Widget)
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
					return true // 等合成器真正进入全屏
				}
				// 校准点击是第一条注入：后端检查必须先于它，补点重试同理。
				if ok, name := gdkBackendIsWayland(); !ok {
					focusAbort = fmt.Sprintf("GDK 后端不是 Wayland（%q），未注入任何事件", name)
					app.Quit()
					return false
				}
				if calibratedAt.IsZero() {
					calibratedAt = time.Now()
					// 校准点击是第一条注入：显式 opt-in + Wayland + 全屏到原点
					// 显示器已把范围压到最小，但无法在注入前证明合成器把触摸
					// 设备映射到哪块输出——这一下仍有落到宿主应用的理论可能，
					// 随后 2 秒内收不到事件就中止。
					// (60,60) 在标题栏/搜索条左侧，任何映射下都只是无害区域。
					touch.tapAt(float64(monX)+60, float64(monY)+60, dX, dY, dW, dH)
					return true
				}
				if calibPressed && calibX > 20 && calibX < 140 && calibY > 20 && calibY < 140 {
					calibrated = true
					u.win.RemoveController(&gesture.EventController)
					return true
				}
				if time.Since(calibratedAt) > 2*time.Second {
					focusAbort = "触摸校准点击没有回到本窗口（输入映射不是整块桌面，或窗口未真正全屏），未继续注入"
					app.Quit()
					return false
				}
				return true
			}

			sw := &u.switcher.Widget
			cx, cy, ok := widgetCenter(sw, &u.win.Widget)
			if !ok {
				return true
			}

			switch step {
			case 0:
				if !injectOK() {
					return false
				}
				// 左侧标签 = 「分身」（空态）。
				touch.tapAt(float64(monX)+cx-float64(sw.GetWidth())/4, float64(monY)+cy, dX, dY, dW, dH)
				step = 1
				stepDeadline = time.Now().Add(4 * time.Second)
			case 1:
				if u.stack.GetVisibleChildName() != "on" {
					return true
				}
				if !injectOK() {
					return false
				}
				// 右侧标签 = 「所有应用」。
				touch.tapAt(float64(monX)+cx+float64(sw.GetWidth())/4, float64(monY)+cy, dX, dY, dW, dH)
				step = 2
				stepDeadline = time.Now().Add(4 * time.Second)
			case 2:
				if u.stack.GetVisibleChildName() != "all" {
					return true
				}
				// 等视图切换动画结束，搜索框停在最终位置再点。
				if allAt.IsZero() {
					allAt = time.Now()
				}
				if time.Since(allAt) < 300*time.Millisecond {
					return true
				}
				if !injectOK() {
					return false
				}
				ex, ey, ok := widgetCenter(&u.search.Widget, &u.win.Widget)
				if !ok {
					return true
				}
				touch.tapAt(float64(monX)+ex, float64(monY)+ey, dX, dY, dW, dH)
				step = 3
				stepDeadline = time.Now().Add(4 * time.Second)
			case 3:
				if !focusWithin(u, &u.search.Widget) {
					return true
				}
				if !injectOK() {
					return false
				}
				// 末尾的 Return 用来提交输入法预编辑（有 IME 时按键会先停在
				// preedit，GetText 看不到）；无 IME 时它只是搜索框的 activate。
				pending = []uint16{keyD, keyE, keyM, keyO, keyReturn}
				step = 4
				stepDeadline = time.Now().Add(2 * time.Second)
			case 4, 6:
				// 每 tick 只发一个键：期间的失焦事件能回到主循环处理，不会在
				// 一次阻塞 120ms 的连发里把剩余按键送进别的窗口。
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
					stepDeadline = time.Now().Add(2 * time.Second)
					return true
				}
				if step == 4 {
					step = 5
					stepDeadline = time.Now().Add(4 * time.Second)
				} else {
					step = 7
					stepDeadline = time.Now().Add(4 * time.Second)
				}
			case 5:
				// 不假设键盘布局（AZERTY 等布局下物理键位产生的字符不同）：
				// 只要求按键确实进了搜索框，退格数按实际字符数生成。
				text := u.search.GetText()
				n := len([]rune(text))
				if n == 0 {
					return true
				}
				// 搜索接线必须真的跑过：u.query 由 search-changed 防抖回调写入，
				// 且产品会 ToLower+TrimSpace（Caps Lock 下输入框是 `DEMO`）。
				// 删掉 ConnectSearchChanged 时这里会超时，而不是被后续退格掩盖。
				if u.query != strings.ToLower(strings.TrimSpace(text)) {
					return true
				}
				// 接线生效还不够：列表必须真的按 query 过滤。输入内容恰好是
				// "alpha" 子串时（不同布局可能产生）旧行保留是合法的，跳过。
				q := strings.ToLower(strings.TrimSpace(text))
				if !strings.Contains("alpha", q) {
					if row := findListRow(u.groups[0], "Alpha"); row != nil {
						row.Unref()
						fail("搜索 %q 后 Alpha 行仍在列表中（过滤没有接线？）", text)
						return false
					}
				}
				if !injectOK() {
					return false
				}
				pending = make([]uint16, n)
				for i := range pending {
					pending[i] = keyBackspace
				}
				step = 6
			case 7:
				if u.search.GetText() != "" || u.query != "" {
					return true
				}
				row := findListRow(u.groups[0], "Alpha")
				if row == nil {
					return true // 防抖落地中
				}
				row.Unref()
				u.win.Unfullscreen()
				step = 8
				app.Quit()
				return false
			}
			return true
		}
		glib.TimeoutAdd(50, &tick, 0)
	}
	app.ConnectActivate(&actcb)
	u.hold(&actcb)

	// activate 没触发时（显示/启动异常）主循环里不会有任何 tick，只能靠
	// 这个外部 watchdog 退出，否则测试要卡到 -timeout。
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
	if !activated || step != 8 {
		t.Fatalf("真实注入用例没有跑完：activated=%t step=%d", activated, step)
	}
}

// detailButton 在详情页里按 tooltip 找唯一一个按钮；页面重建中可能暂时没有。
func detailButton(v *detailView, tip string) *gtk.Widget {
	if v == nil || !v.loaded || v.content == nil {
		return nil
	}
	ws := findWidgets(&v.content.Widget, func(w *gtk.Widget) bool {
		return w.GetTooltipText() == tip
	})
	if len(ws) != 1 {
		return nil
	}
	return ws[0]
}

// TestGtkRealInputStartStop 用真实触摸事件点「启动」和「停止」：
// 不是调 v.launch()/v.stop()，而是让 uinput 事件穿过 libinput → GDK → 按钮，
// 再走 detached runner、core.Run、run.lock 与 core.StopRun 的完整链路。
//
// 为了不拉起用户自己的应用，被测实例的 Exec 指向假 HOME 里一个只写自己配置、
// 然后 sleep 30 的脚本；整个用例在假 HOME/AC_BASE 下运行，结束（含失败）后
// 主动 StopRun 收尾。停止确认对话框的响应走产品同一个 dialogFn（对话框本身的
// 默认响应/接线已由 TestGtkDialogResponses 覆盖），这里验的是按钮点击与
// 运行态切换。
func TestGtkRealInputStartStop(t *testing.T) {
	skipUnlessUinput(t)
	bin := os.Getenv("APPCLONE_GTK_GUI_BIN")
	if bin == "" {
		t.Skip("需要 APPCLONE_GTK_GUI_BIN 指向 appclone-gui（design/guitest-gtk.sh 会构建）")
	}

	env, appsDir := gtkTestEnv(t, "uinputstart")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(os.Getenv("HOME"), "keepalive.sh")
	body := "#!/bin/sh\nmkdir -p \"$HOME/.config/Demo\"\necho run >> \"$HOME/.config/Demo/state\"\nexec /bin/sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: script,
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "manual",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}
	if _, err := env.CreateInstance(context.Background(), "demo", profile, "main"); err != nil {
		t.Fatal(err)
	}
	// 失败/中止时 runner 可能还在 sleep：用例退出前必须收掉，避免留下实例锁。
	// 点过启动但 runner 还没拿到 run.lock 的窗口里失败时，IsRunning 仍为 false，
	// 因此先等它现身；已经走完停止流程（stopped）或从未点启动则直接返回。
	// 已知残余：runner 刚持锁、run.pid 尚未写下的微秒级窗口里 StopRun 会返回
	// stop_no_pid（不盲杀），进程最迟 sleep 30 自行退出，不会再拉起应用。
	sawLaunch, sawStop, stopped := false, false, false
	defer func() {
		if !sawLaunch || stopped {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deadline := time.Now().Add(4 * time.Second)
		for {
			if running, _ := env.IsRunning("demo", "main"); running {
				if err := env.StopRun(ctx, "demo", "main"); err != nil {
					t.Logf("收尾 StopRun 失败（runner 可能仍在退出）：%v", err)
				}
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	oldExe := detachedExecutable
	detachedExecutable = func() (string, error) { return bin, nil }
	defer func() { detachedExecutable = oldExe }()

	oldHook := dialogHook
	dialogs := make(chan *adw.AlertDialog, 1)
	dialogHook = func(d *adw.AlertDialog) {
		select {
		case dialogs <- d:
		default:
		}
	}
	defer func() { dialogHook = oldHook }()

	touch, err := newTouchscreen()
	if err != nil {
		t.Fatalf("创建虚拟触摸屏失败（%v）；真实注入用例无法运行", err)
	}
	defer touch.Close()

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkStartStop",
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
	injectOK := func() bool {
		if u.win != nil && u.win.IsActive() && u.win.IsFullscreen() {
			// WAYLAND_DISPLAY 可能从宿主继承（例如 Wayland 桌面里跑 xvfb-run），
			// 不能只靠环境变量判定：真正决定事件去哪的是 GDK 后端。X11/Xvfb
			// 下 uinput 事件进宿主 seat，可能落到别的应用上，一律不注入。
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

	calibrated := false
	calibratedAt := time.Time{}
	calibPressed := false
	calibTaps := 0
	var calibX, calibY float64
	fullscreenRequested := false
	fullscreenAt := time.Time{}
	envReadyAt := time.Time{}
	detailAt := time.Time{}
	pushCount := 0
	lastBranch := "start"
	launchedAt := time.Time{}
	stoppedAt := time.Time{}
	stopHit := ""
	stopTaps := 0
	lastStopTap := time.Time{}
	stopCx, stopCy := 0.0, 0.0
	dialogDone := false
	stopSeen := false
	stopClicked := false
	stopHasFn := false
	stopSensitive := false
	stopMapped := false
	var dX, dY, dW, dH int32
	var monX, monY int32

	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		activated = true
		u.buildWindow()
		u.win.Present()

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

		deadline := time.Now().Add(60 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				v := u.activeDetail
				state := "detail=nil"
				if v != nil {
					state = fmt.Sprintf("detailLoaded=%t sig=%q insts=%d", v.loaded, v.sig, len(v.insts))
				}
				fail("真实启动/停止用例超时（launch=%t stop=%t stopped=%t calibrated=%t pushes=%d branch=%s %s）",
					sawLaunch, sawStop, stopped, calibrated, pushCount, lastBranch, state)
				return false
			}
			if !u.loaded || !u.envChecked || !u.win.GetMapped() {
				lastBranch = "wait-loaded"
				return true
			}
			if !fullscreenRequested {
				fullscreenAt = time.Now()
				u.win.FullscreenOnMonitor(monitorAtOrigin())
				fullscreenRequested = true
				return true
			}
			// 点了「停止」之后确认框会成为活动窗口，u.win 失去焦点是预期行为；
			// 此时不再注入，只等 dialogHook/StopRun 的结果。
			if !sawStop && !u.win.IsActive() {
				lastBranch = "inactive"
				if time.Since(fullscreenAt) > 6*time.Second {
					focusAbort = "窗口在 6 秒内没有获得焦点，未注入任何事件"
					app.Quit()
					return false
				}
				u.win.Present()
				return true
			}
			mon := windowMonitor(u)
			if mon == nil {
				lastBranch = "no-monitor"
				return true
			}
			var geo gdk.Rectangle
			mon.GetGeometry(&geo)
			if u.win.GetWidth() < geo.Width-2 {
				lastBranch = "narrow"
				return true
			}
			if envReadyAt.IsZero() {
				envReadyAt = time.Now()
			}
			if time.Since(envReadyAt) < 400*time.Millisecond {
				lastBranch = "env-settle"
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

			// 校准必须在列表页完成：(60,60) 落在标题栏，不会碰到详情页左上角的
			// 返回按钮。校准后才推详情页。
			if !calibrated {
				if !u.win.IsFullscreen() {
					return true
				}
				// 校准点击（含补点）是第一批注入：后端检查必须先于它们。
				if ok, name := gdkBackendIsWayland(); !ok {
					focusAbort = fmt.Sprintf("GDK 后端不是 Wayland（%q），未注入任何事件", name)
					app.Quit()
					return false
				}
				if calibratedAt.IsZero() {
					calibratedAt = time.Now()
					touch.tapAt(float64(monX)+60, float64(monY)+60, dX, dY, dW, dH)
					calibTaps++
					return true
				}
				if calibPressed && calibX > 20 && calibX < 140 && calibY > 20 && calibY < 140 {
					calibrated = true
					u.win.RemoveController(&gesture.EventController)
					return true
				}
				if time.Since(calibratedAt) > 1*time.Second && calibTaps < 3 {
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

			if u.activeDetail == nil {
				lastBranch = "push-detail"
				if pushCount > 0 {
					fail("详情页被反复打开/弹出（第 %d 次 push）", pushCount+1)
					return false
				}
				u.pushDetail("demo")
				pushCount++
				detailAt = time.Now()
				return true
			}
			v := u.activeDetail
			if v == nil || !v.loaded {
				lastBranch = "detail-loading"
				return true
			}
			if time.Since(detailAt) < 400*time.Millisecond {
				lastBranch = "detail-settle"
				return true
			}

			if !sawLaunch {
				btn := detailButton(v, u.tr("ui.instance.launch.tooltip", nil))
				if btn == nil {
					lastBranch = "no-launch-btn"
					if time.Since(detailAt) > 10*time.Second {
						fail("详情页 10 秒内没有「启动」按钮（sig=%q insts=%d）", v.sig, len(v.insts))
						return false
					}
					return true
				}
				cx, cy, ok := widgetCenter(btn, &u.win.Widget)
				if !ok {
					lastBranch = "no-launch-geo"
					if time.Since(detailAt) > 10*time.Second {
						fail("「启动」按钮坐标不可用（w=%d h=%d mapped=%t visible=%t loaded=%t）",
							btn.GetWidth(), btn.GetHeight(), btn.GetMapped(), visibleIsPage(u, v.page), v.loaded)
						return false
					}
					return true
				}
				if !injectOK() {
					return false
				}
				touch.tapAt(float64(monX)+cx, float64(monY)+cy, dX, dY, dW, dH)
				sawLaunch = true
				launchedAt = time.Now()
				return true
			}

			if !sawStop {
				running, rerr := env.IsRunning("demo", "main")
				if rerr != nil {
					fail("读取运行态失败：%v", rerr)
					return false
				}
				if !running {
					if time.Since(launchedAt) > 15*time.Second {
						fail("点「启动」后 15 秒内 runner 没有持有 run.lock（启动失败？）")
						return false
					}
					return true
				}
				// 详情页每秒刷新一次运行态；等「启动」换成「停止」再点。
				btn := detailButton(v, u.tr("ui.instance.stop.tooltip", nil))
				if btn == nil {
					if time.Since(launchedAt) > 15*time.Second {
						fail("实例已在运行，但「停止」按钮 15 秒内没有出现")
						return false
					}
					return true
				}
				cx, cy, ok := widgetCenter(btn, &u.win.Widget)
				if !ok {
					return true
				}
				stopSensitive, stopMapped = btn.GetSensitive(), btn.GetMapped()
				stopHasFn = u.cb.action[btn.GoPointer()] != nil
				var ccb func(gtk.Button)
				ptr := btn.GoPointer()
				ccb = func(gtk.Button) {
					stopClicked = true
					stopHasFn = u.cb.actionHandler(ptr) != nil
				}
				gtk.ButtonNewFromInternalPtr(btn.GoPointer()).ConnectClicked(&ccb)
				u.hold(&ccb)
				g := gtk.NewGestureClick()
				g.SetPropagationPhase(gtk.PhaseCaptureValue)
				var gcb func(gtk.GestureClick, int32, float64, float64)
				gcb = func(gtk.GestureClick, int32, float64, float64) { stopSeen = true }
				g.ConnectPressed(&gcb)
				btn.AddController(&g.EventController)
				u.hold(&gcb, g)
				if w := u.win.Widget.Pick(cx, cy, gtk.PickDefaultValue); w != nil {
					for ; w != nil; w = w.GetParent() {
						if tip := w.GetTooltipText(); tip != "" {
							stopHit = tip
							break
						}
					}
				}
				if !injectOK() {
					return false
				}
				touch.tapAt(float64(monX)+cx, float64(monY)+cy, dX, dY, dW, dH)
				stopCx, stopCy = cx, cy
				stopTaps++
				lastStopTap = time.Now()
				sawStop = true
				stoppedAt = time.Now()
				return true
			}

			// 停止按钮已点：对话框经 product 的 dialogHook 交出，响应走同一个
			// dialogFn（产品回调）。对话框会拿到焦点，之后不再注入任何事件。
			if !dialogDone {
				select {
				case d := <-dialogs:
					if got := d.GetDefaultResponse(); got != "cancel" {
						fail("停止确认框默认响应应为 cancel，得到 %q", got)
						d.Close()
						return false
					}
					// dialogFn 与该对话框的 onDialog 登记的是同一张表：这里直接
					// 投递响应，等价于用户按下「停止」，随后关掉对话框窗口。
					u.cb.dialogFn(*d, "stop")
					d.Close()
					dialogDone = true
				default:
					if stopTaps < 3 && time.Since(lastStopTap) > 1*time.Second && u.win.IsActive() && u.win.IsFullscreen() {
						// 触摸事件从合成器回到控件有延迟，期间详情页可能正好重建；
						// 同一坐标补点，最多三次。确认框一出现就不再补点。
						touch.tapAt(float64(monX)+stopCx, float64(monY)+stopCy, dX, dY, dW, dH)
						stopTaps++
						lastStopTap = time.Now()
						return true
					}
					if time.Since(stoppedAt) > 6*time.Second {
						fail("点「停止」%d 次后仍没有出现确认框（落点 tooltip=%q seen=%t clicked=%t hasFn=%t sensitive=%t mapped=%t push=%d）", stopTaps, stopHit, stopSeen, stopClicked, stopHasFn, stopSensitive, stopMapped, pushCount)
						return false
					}
					return true
				}
			}

			running, rerr := env.IsRunning("demo", "main")
			if rerr != nil {
				fail("读取运行态失败：%v", rerr)
				return false
			}
			if running {
				if time.Since(stoppedAt) > 15*time.Second {
					fail("确认停止后 15 秒内 run.lock 仍被持有")
					return false
				}
				return true
			}
			// 只断言文件存在性，不读内容：state 是分身自己的数据。
			// bwrap 把 paths 映射到 <inst>/root + 原绝对路径。宿主目录本身由
			// CreateInstance 预建（让应用启动时看到目标存在），但其中的 state
			// 只能出现在实例根里。
			hostCfg := filepath.Join(os.Getenv("HOME"), ".config", "Demo")
			stateFile := filepath.Join(env.InstancePath("demo", "main"), "root"+hostCfg, "state")
			if _, err := os.Stat(stateFile); err != nil {
				fail("实例根里没有应用写出的 state（%v）：runner 可能没在沙箱内运行", err)
				return false
			}
			if _, err := os.Stat(filepath.Join(hostCfg, "state")); err == nil {
				fail("宿主假 HOME 出现 state：应用数据写到了真实目录而不是实例根")
				return false
			}
			stopped = true
			t.Logf("真实停止链路完成：stopTaps=%d stopSeen=%t stopClicked=%t", stopTaps, stopSeen, stopClicked)
			u.win.Unfullscreen()
			app.Quit()
			return false
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
	if !sawLaunch || !sawStop || !stopped {
		t.Fatalf("真实启动/停止没有跑完：launch=%t stop=%t stopped=%t", sawLaunch, sawStop, stopped)
	}
}
