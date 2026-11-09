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
)

// widgetCenter 返回控件中心在 toplevel 坐标系里的位置。
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

func TestGtkRealInputUinput(t *testing.T) {
	if os.Getenv("APPCLONE_GTK_UINPUT") != "1" {
		t.Skip("真实输入注入需要可写的 /dev/uinput；design/guitest-gtk.sh 会在具备条件时打开")
	}
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
	layoutAt := time.Time{}
	allAt := time.Time{}
	var dX, dY, dW, dH int32
	var monX, monY int32

	// injectOK 在每次注入前复查焦点：窗口不是活动窗口时绝不发事件，
	// 避免把点击/按键送进用户自己的应用。
	injectOK := func() bool {
		if u.win != nil && u.win.IsActive() && u.win.IsFullscreen() {
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
			if !u.loaded || !u.stack.GetMapped() || !u.win.GetMapped() {
				return true
			}
			if !fullscreenRequested {
				u.win.FullscreenOnMonitor(monitorAtOrigin())
				fullscreenRequested = true
				return true
			}
			if !u.win.IsActive() {
				if !fullscreenAt.IsZero() && time.Since(fullscreenAt) > 6*time.Second {
					focusAbort = "窗口在 6 秒内没有获得焦点，未注入任何事件"
					app.Quit()
					return false
				}
				u.win.Present()
				return true
			}
			if fullscreenAt.IsZero() {
				fullscreenAt = time.Now()
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

			if !calibrated {
				if !u.win.IsFullscreen() {
					return true // 等合成器真正进入全屏
				}
				if calibratedAt.IsZero() {
					calibratedAt = time.Now()
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
