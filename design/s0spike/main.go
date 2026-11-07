// S0 spike：用最少代码验证 puregotk 是否满足 appclone GUI 的五条准入。
//
// 1. AdwSwitchRow 的 active 变化能进 Go 回调
// 2. AdwNavigationView push/pop，且 Esc 能返回
// 3. AdwAlertDialog 的 response id 能拿到（异步）
// 4. 中文渲染 + 系统暗色可读
// 5. 在 Ubuntu 24.04（GTK 4.14 / libadwaita 1.5）上跑通 —— 需另一台机器/容器
//
// 构建：CGO_ENABLED=0 go build -o s0 .
// 运行：./s0（需要图形会话）；交互后用终端输出核对五条。
// 手动模式（验证 Esc 语义）：S0_MANUAL=1 ./s0，再在 X11 下 xdotool key Escape。
// Ubuntu 24.04 复核（libadwaita 1.5.0 / GTK 4.14）：容器内装 libgtk-4-1、
// libadwaita-1-0、xvfb 后 `xvfb-run -a ./s0`，期望退出码 0 且 S0-1/2/3 都有输出。
package main

import (
	"fmt"
	"os"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"
)

func main() {
	app := adw.NewApplication("io.github.mensylisir.AppcloneS0", gio.GApplicationFlagsNoneValue)
	defer app.Unref()
	actcb := func(_ gio.Application) { activate(app) }
	app.ConnectActivate(&actcb)
	if code := app.Run(int32(len(os.Args)), os.Args); code > 0 {
		os.Exit(int(code))
	}
}

var (
	keep          []any // 防 GC 回收 Go 闭包（puregotk 无 finalizer）
	currentDialog *adw.AlertDialog
)

func activate(app *adw.Application) {
	win := adw.NewApplicationWindow(&app.Application)
	win.SetTitle("分身")
	win.SetDefaultSize(520, 640)

	// ④ 中文 + 系统暗色：把实际取到的状态打出来，暗色与否由桌面的
	// color-scheme 决定（prefer-dark），不是本程序硬编码。
	sm := app.GetStyleManager()
	fmt.Printf("S0-4 中文标题=分身 dark=%v high_contrast=%v\n", sm.GetDark(), sm.GetHighContrast())

	// ① SwitchRow：connect notify，任何属性变化都回读 active。
	// 真实实现要按 pspec 过滤，spike 只需确认 GObject 信号能进 Go。
	row := adw.NewSwitchRow()
	row.SetTitle("微信 · 分身 1")
	row.SetSubtitle("隔离 ~/.config/WeChat 与 ~/.local/share/WeChat")
	last := row.GetActive()
	notifycb := func(_ gobject.Object, _ uintptr) {
		if cur := row.GetActive(); cur != last {
			last = cur
			fmt.Printf("S0-1 switch active=%v\n", cur)
		}
	}
	row.ConnectNotify(&notifycb)

	// ③ AlertDialog：异步 response id。
	btnDialog := gtk.NewButtonWithLabel("打开确认框")
	openDialog := func() {
		dlg := adw.NewAlertDialog("移到回收站？", "删除后可从回收站恢复；永久删除只能在回收站里手动完成。")
		dlg.AddResponse("cancel", "取消")
		dlg.AddResponse("trash", "移到回收站")
		dlg.SetDefaultResponse("cancel")
		respcb := func(_ adw.AlertDialog, resp string) {
			fmt.Printf("S0-3 alert response=%q\n", resp)
		}
		hid := dlg.ConnectResponse(&respcb)
		fmt.Printf("S0-3 response handler=%d\n", hid)
		keep = append(keep, dlg, &respcb)
		currentDialog = dlg
		dlg.Choose(&win.Widget, nil, nil, 0)
	}
	dlgcb := func(_ gtk.Button) { openDialog() }
	btnDialog.ConnectClicked(&dlgcb)

	// ② NavigationView：push 第二页，Esc 或返回按钮 pop。
	nav := adw.NewNavigationView()

	btnPush := gtk.NewButtonWithLabel("进入详情")
	pushPage := func() {
		box := gtk.NewBox(gtk.OrientationVerticalValue, 12)
		box.SetMarginTop(24)
		box.SetMarginBottom(24)
		box.SetMarginStart(24)
		box.SetMarginEnd(24)
		label := gtk.NewLabel("详情页——按 Esc 应返回")
		box.Append(&label.Widget)
		back := gtk.NewButtonWithLabel("返回")
		backcb := func(_ gtk.Button) {
			ok := nav.Pop()
			fmt.Printf("S0-2 pop ok=%v\n", ok)
		}
		back.ConnectClicked(&backcb)
		box.Append(&back.Widget)
		keep = append(keep, &backcb, box, label, back)
		nav.Push(adw.NewNavigationPage(&box.Widget, "详情"))
		fmt.Println("S0-2 pushed")
	}
	pushcb := func(_ gtk.Button) { pushPage() }
	btnPush.ConnectClicked(&pushcb)
	poppedcb := func(_ adw.NavigationView, _ uintptr) {
		fmt.Println("S0-2 popped（Esc 也会走这里）")
	}
	nav.ConnectPopped(&poppedcb)

	actions := gtk.NewBox(gtk.OrientationVerticalValue, 12)
	actions.SetMarginTop(12)
	actions.SetMarginBottom(12)
	actions.SetMarginStart(12)
	actions.SetMarginEnd(12)
	actions.Append(&btnDialog.Widget)
	actions.Append(&btnPush.Widget)

	// 页面 1 = SwitchRow + 操作按钮：用 vertical box 包住再放进 NavigationPage。
	pageBox := gtk.NewBox(gtk.OrientationVerticalValue, 0)
	pageBox.Append(&row.Widget)
	pageBox.Append(&actions.Widget)
	nav.Push(adw.NewNavigationPage(&pageBox.Widget, "应用"))

	tv := adw.NewToolbarView()
	tv.AddTopBar(&adw.NewHeaderBar().Widget)
	tv.SetContent(&nav.Widget)

	win.SetContent(&tv.Widget)
	win.Present()

	// 自驱动：没有输入设备也能证明 notify/push/pop 这三条绑定真的通。
	// 600ms 一步，四步后退出；打印里能看到 S0-1 / S0-2。
	step := 0
	drivecb := func(uintptr) bool {
		step++
		switch step {
		case 1:
			fmt.Println("S0-1 设置 active=true（应触发 notify 回调）")
			row.SetActive(true)
		case 2:
			openDialog()
		case 3:
			// 没有点击设备时，直接发 response 信号验证参数编组与分发；
			// 真实点击路径仍要人手验一次（S0 第 3 条的 UI 部分）。
			if currentDialog != nil {
				gobject.SignalEmitByName(&currentDialog.Widget.Object, "response", "trash")
			}
		case 4:
			pushPage()
		case 5:
			fmt.Printf("S0-2 主动 pop ok=%v\n", nav.Pop())
		case 6:
			app.Quit()
			return false
		}
		return true
	}
	drivefn := glib.SourceFunc(drivecb)
	keep = append(keep, &notifycb, &dlgcb, &pushcb, &poppedcb, &drivefn, row, nav, win, btnDialog, btnPush, actions, pageBox, tv)
	if os.Getenv("S0_MANUAL") != "1" {
		glib.TimeoutAdd(600, &drivefn, 0)
	} else {
		// 手动模式：先 push 一页，留给外部 xdotool 发 Esc 验证语义。
		pushPage()
	}
}
