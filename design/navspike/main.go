// navspike：验证 appclone GUI 的导航页生命周期假设（P3-1 修复的前提）。
//
// 假设：`adw_navigation_view_push` 的 page 是 transfer none（nav 自己引用），
// 而 puregotk 构造器已经 ref_sink 过一次；popped 信号发出后（页面真的离开
// 导航栈）把这份 sink 引用 Unref 掉是安全的，而且延后一个 idle 更稳。
//
// 本程序不接收输入，自己 push/pop 40 次；任何一次 Unref 过早都会让 GTK
// 在 pop 动画或下轮 push 时崩溃/断言，退出码非 0。
//
// 构建：CGO_ENABLED=0 go build -o navspike ./design/navspike
// 运行：GDK_BACKEND=x11 DISPLAY=:0 ./navspike
package main

import (
	"fmt"
	"os"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"
)

var (
	pages  = map[uintptr]*adw.NavigationPage{} // 还在导航栈上的页
	owned  = map[uintptr]*adw.NavigationPage{} // 我们持有 sink 引用的页（含已 pop 待释放）
	unrefQ []uintptr
	respFn glib.SourceFunc
)

func main() {
	app := adw.NewApplication("io.github.mensylisir.AppcloneNavSpike", gio.GApplicationFlagsNoneValue)
	defer app.Unref()
	actcb := func(_ gio.Application) { activate(app) }
	app.ConnectActivate(&actcb)
	if code := app.Run(int32(len(os.Args)), os.Args); code > 0 {
		os.Exit(int(code))
	}
}

func activate(app *adw.Application) {
	win := adw.NewApplicationWindow(&app.Application)
	win.SetTitle("navspike")
	win.SetDefaultSize(360, 240)
	nav := adw.NewNavigationView()
	win.SetContent(&nav.Widget)

	// 与 ui.releasePage 相同：popped 之后延后一个 idle 再 Unref 我们持有的
	// 那份 sink 引用。若假设不成立，这 40 次 push/pop 里必然出事。
	poppedcb := func(_ adw.NavigationView, ptr uintptr) {
		if _, ok := pages[ptr]; ok {
			delete(pages, ptr)
			unrefQ = append(unrefQ, ptr)
			glib.IdleAdd(&respFn, 0)
		}
	}
	nav.ConnectPopped(&poppedcb)
	respFn = func(uintptr) bool {
		if len(unrefQ) == 0 {
			return false
		}
		ptr := unrefQ[0]
		unrefQ = unrefQ[1:]
		if p, ok := owned[ptr]; ok {
			delete(owned, ptr) // 先把表项摘掉，Unref 之后包装指针即失效
			p.Unref()
		}
		return false
	}

	push := func(i int) {
		box := gtk.NewBox(gtk.OrientationVerticalValue, 6)
		label := gtk.NewLabel(fmt.Sprintf("第 %d 页", i))
		box.Append(&label.Widget)
		page := adw.NewNavigationPage(&box.Widget, fmt.Sprintf("页 %d", i))
		pages[page.GoPointer()] = page
		owned[page.GoPointer()] = page
		nav.Push(page)
	}

	win.Present()

	// 交替 push / pop：给 pop 动画留一拍，再压下一页。
	step := 0
	var tick glib.SourceFunc
	tick = func(uintptr) bool {
		switch {
		case step >= 80:
			fmt.Println("navspike: 40 轮 push/pop 完成，无崩溃")
			app.Quit()
			return false
		case step%2 == 0:
			push(step / 2)
		default:
			nav.Pop()
		}
		step++
		return true
	}
	glib.TimeoutAdd(120, &tick, 0)
}
