//go:build linux && gtk_e2e

package main

// GTK 侧回归（E2E）：在真实 GTK/libadwaita 里走一遍"列表 → 配置确认 →
// 创建实例 → 详情 → 返回"的生命周期。
//
// 构建标签 `gtk_e2e` 是刻意的：puregotk 在包 init 阶段就要 dlopen
// libgtk/libadwaita，没有 GTK 运行库的机器上 `go test ./...` 会在 init 阶段
// panic，`t.Skip` 根本来不及执行。默认门禁（core-only 容器）不编译这个文件；
// `design/guitest-gtk.sh` 用 `-tags gtk_e2e` 显式把它编进来。
//
// 为什么不用合成输入：本机会话禁止 XTEST 注入（xdotool 无效），broadway 在
// push 后还会以 gdk/broadway 自身断言自崩。但产品的入口就在信号回调里——
// 拨 SwitchRow 的 active 会发真实 notify::active 进 u.onSwitch；确认页
// 直调的是按钮 clicked 绑定的同一个 submit 处理函数。这是直调回调，不是
// 合成点击，不需要指针事件。
import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

// noFuseMounter：闸门只验页面生命周期，不该在 $TMPDIR 做一次真实
// fuse-overlay 冒烟挂载（checkEnv 会调 Available）。报"不可用"让界面走
// 降级分支，同时证明降级不挡这条流程。
type noFuseMounter struct{}

func (noFuseMounter) Available() error { return errors.New("gtk e2e：不做 fuse 冒烟") }

func (noFuseMounter) Mount(string, string, string, string) (func() error, error) {
	return nil, errors.New("gtk e2e：不做挂载")
}

// gtkTestEnv 建一套隔离的假 HOME / XDG / AC_BASE，返回 Env 和只属于本次
// 测试的 .desktop 目录。
//
// 假 HOME 是硬要求：回归不允许碰真实主目录（CLAUDE.md 冒烟规则）。AC_HOME
// 必须一起钉住：PathResolver 用 AC_HOME 作为可见 Home，用 $HOME 作为
// RealHome；只钉 AC_HOME 会让 bwrap 的受限目录仍指向真实主目录。
//
// 根目录优先取脚本给的 $APPCLONE_GTK_ROOT（放在 $WORK 下，--keep 时保留
// 失败现场）；直接 go test 时退回 t.TempDir（测试结束自动清理）。
func gtkTestEnv(t *testing.T, name string) (*core.Env, string) {
	if os.Getenv("APPCLONE_GTK_TEST") != "1" {
		t.Skip("GTK 回归需要显式开关：design/guitest-gtk.sh")
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("没有可用的显示（DISPLAY / WAYLAND_DISPLAY）")
	}

	root := os.Getenv("APPCLONE_GTK_ROOT")
	if root == "" {
		root = t.TempDir()
	} else {
		root = filepath.Join(root, name)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(root, "home")
	envs := map[string]string{
		"HOME":            home,
		"AC_HOME":         home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"XDG_RUNTIME_DIR": filepath.Join(root, "run"),
		"AC_BASE":         filepath.Join(root, "xdg", "appclone"),
	}
	for k, v := range envs {
		t.Setenv(k, v)
		if err := os.MkdirAll(v, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	env, err := core.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	// 只看自己写的 .desktop：扫描真实 /usr/share 会让闸门随机器漂移
	// （本机 269 个应用，分页/排序都会影响断言）。
	appsDir := filepath.Join(envs["XDG_DATA_HOME"], "applications")
	if err := os.MkdirAll(appsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env.AppDirList = []string{appsDir}
	env.Mounter = noFuseMounter{}
	return env, appsDir
}

func TestGtkScreensLifecycle(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "lifecycle")

	// 假应用：CreateInstance 会先 LoadApp 确认应用存在（GUI 与 CLI 同一条校验）。
	for name, body := range map[string]string{
		"demo.desktop":  "[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n",
		"alpha.desktop": "[Desktop Entry]\nType=Application\nName=Alpha\nExec=/usr/bin/true\n",
	} {
		if err := os.WriteFile(filepath.Join(appsDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 预置一份可复用档案：open 入口不跑探测（探测要真启动应用，属于冒烟脚本
	// 的范围），回归只验屏幕生命周期与落盘结果。
	if err := env.SaveProfile(core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "overlay",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	// NonUnique：并发跑两次闸门、或上一次运行被中断留下进程时，唯一实例的
	// GApplication 会把 activate 转发给主实例并让本进程"成功"退出——断言
	// 一次都不执行，测试假通过（复审 P1）。
	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkTest",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	u := &ui{app: app, env: env, lang: "zh_CN"}
	var runErr error
	activated := false
	step := 0
	submittedAt := time.Time{}
	var detailGoneAt time.Time
	var firstSetup *setupView
	fail := func(format string, a ...any) {
		if runErr == nil {
			runErr = fmt.Errorf(format, a...)
		}
		app.Quit()
	}

	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		activated = true
		u.buildWindow()
		deadline := time.Now().Add(45 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				fail("GTK 回归超时：状态机没有在 45 秒内推进（停在第 %d 步）", step)
				return false
			}
			switch step {
			case 0:
				// 首屏扫描是异步的：等它落到界面上再判定。
				if !u.loaded {
					return true
				}
				// 视图栈必须真的画出来：GtkWidget:visible 是局部属性，祖先隐藏时
				// 仍为 true——只断 GetVisible 抓不到"首轮落地后忘记恢复显示"的
				// 永久空白（把 renderList 末尾的 SetVisible(true) 删掉，断言要变红）。
				if !u.stack.GetVisible() {
					fail("首轮扫描落地后视图栈仍不可见")
					return false
				}
				if !u.stack.GetMapped() {
					return true // 等窗口映射完再判定；一直不映射会由总超时判红
				}
				if !u.switcher.GetSensitive() {
					fail("首轮落地后视图切换器仍被禁用")
					return false
				}
				// 空态不是 PreferencesGroup（Group 只收 AdwPreferencesRow，
				// StatusPage 塞进去会被裁成半截图标），它挂在「分身」视图里。
				// 空态时列表只剩「所有应用」一组。
				if len(u.groups) != 1 {
					fail("空态下列表应只有「所有应用」1 组，得到 %d 组", len(u.groups))
					return false
				}
				if u.emptyPage == nil || !u.emptyPage.GetVisible() {
					fail("空态没有显示出来")
					return false
				}
				if p := u.emptyPage.GetParent(); p == nil {
					fail("空态页没有挂进窗口")
					return false
				} else {
					p.Unref()
				}
				// 一个分身都没有时首屏落在「所有应用」：点亮开关是唯一能做的事。
				// 空态在「分身」视图里，用户点切换器或空态里的「选择应用」过去。
				if got := u.stack.GetVisibleChildName(); got != "all" {
					fail("没有分身时首屏应落在「所有应用」，实际视图 %q", got)
					return false
				}
				// 搜索过滤必须有牙：query 生效后 Demo 在、Alpha 不在。直接删掉
				// renderList 里的过滤条件会让这条断言变红。
				u.query = "demo"
				u.renderList()
				if alpha := findListRow(u.groups[0], "Alpha"); alpha != nil {
					alpha.Unref()
					fail("搜索「demo」后还渲染了不匹配的 Alpha")
					return false
				}
				row := findListRow(u.groups[0], "Demo")
				if row == nil {
					fail("搜索「demo」后找不到 Demo 行")
					return false
				}
				// 走用户的真实入口：拨开关 → notify::active → u.onSwitch。
				setBool(row, "active", true)
				row.Unref() // GetRow 返回带引用的包装
				step++
			case 1:
				v := u.activeSetup
				if v == nil {
					return true
				}
				switch v.state {
				case "error":
					fail("开启分身落到错误态：%s", v.errPage.GetDescription())
					return false
				case "confirm":
					// 行必须真的进了控件树：标题是候选路径，parent 非 nil 才
					// 说明 group.Add 没漏（v.rows 只是数据 map，漏 Add 时
					// 用户看到的是空确认页，数据却一切正常）。
					ok := false
					for _, row := range v.rows {
						if row.GetTitle() != "~/.config/Demo" {
							continue
						}
						if p := row.GetParent(); p != nil {
							p.Unref()
							ok = true
							break
						}
					}
					if !ok {
						fail("确认页没有把路径行挂出来（标题应为 ~/.config/Demo）")
						return false
					}
					submittedAt = time.Now()
					firstSetup = v
					v.submit()
					step++
				}
			case 2:
				// 创建是事务：等探测页自己 pop 掉（popped 收尾跑完），
				// 再由 beginEnable 的回调落到详情页。
				if v := u.activeSetup; v != nil {
					// 不是原来那一页 = 补发的 notify 又开了配置页；直接判红，
					// 不要让它熬到 45 秒超时才暴露（去重变异测试的证据）。
					if firstSetup != nil && v != firstSetup {
						fail("开关补发的 notify 又开了一个配置页（去重失效）")
						return false
					}
					if v.state == "error" {
						fail("创建落到错误态：%s", v.errPage.GetDescription())
						return false
					}
					return true
				}
				insts, lerr := u.env.ListInstances("demo")
				if lerr != nil || len(insts) != 1 {
					fail("创建后应有 1 个实例，得到 %d 个（%v）", len(insts), lerr)
					return false
				}
				if _, serr := os.Stat(u.env.LauncherPath("demo", insts[0].ID)); serr != nil {
					fail("没有生成桌面入口：%v", serr)
					return false
				}
				step++
			case 3:
				v := u.activeDetail
				if v == nil {
					return true // 详情页由 onClose 回调 push，晚一拍
				}
				if !v.loaded {
					return true
				}
				// 开关的补发 notify（动画结束时）必须被去重：这里迟一点再判定，
				// 给补发留出到达窗口。没有去重时会新开一个配置页。
				if time.Since(submittedAt) < 400*time.Millisecond {
					return true
				}
				if u.activeSetup != nil {
					fail("开关补发的 notify 又开了一个配置页（去重失效）")
					return false
				}
				if len(v.insts) != 1 || len(v.profile.Paths) != 1 {
					fail("详情页数据不对：实例 %d 个、隔离路径 %d 条", len(v.insts), len(v.profile.Paths))
					return false
				}
				if title := v.content.GetTitle(); title != "Demo" {
					fail("详情页标题应为「Demo」，得到 %q", title)
					return false
				}
				// 等详情页真的是可见页再返回；push 动画没结束就 Pop 会被忽略。
				if !visibleIsPage(u, v.page) {
					return true
				}
				u.nav.Pop()
				step++
			case 4:
				if u.activeDetail != nil {
					return true
				}
				// 返回后再等一小段：晚到的补发 notify 若又开了配置页，
				// 这里能抓住；只等 activeDetail 归零就 Quit 会漏掉它。
				if detailGoneAt.IsZero() {
					detailGoneAt = time.Now()
					return true
				}
				if time.Since(detailGoneAt) < 300*time.Millisecond {
					return true
				}
				if u.activeSetup != nil {
					fail("返回列表后又冒出了配置页（补发 notify 去重失效）")
					return false
				}
				// 创建完切回「分身」：返回时刚建出的实例就在眼前，
				// 而不是回到「所有应用」找那个已经不在列表里的开关。
				if got := u.stack.GetVisibleChildName(); got != "on" {
					fail("创建后应切回「分身」视图，实际 %q", got)
					return false
				}
				step++
				app.Quit()
				return false
			}
			return true
		}
		glib.TimeoutAdd(50, &tick, 0)
	}
	app.ConnectActivate(&actcb)
	u.hold(&actcb)

	// 只把程序名交给 GApplication：go test 的 -test.* 参数会被 GLib 当成
	// 未知选项直接退出（"Unknown option -test.paniconexit0"）。
	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK 主循环退出码 %d", code)
	}
	// activate 没跑（例如 GApplication 把本次调用转发给了别的实例）时，
	// runErr 为空、退出码为 0——只靠这两项会假通过。
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !activated || step != 5 {
		t.Fatalf("GTK 回归没有跑完：activated=%t step=%d", activated, step)
	}
}

// TestGtkLandsOnClonesView：已有实例时首屏必须直接落在「分身」。
//
// 单独成测的原因：生命周期用例的首屏没有实例，只证明了"无实例 → 所有应用"；
// 而 buildWindow 的默认值本来就是 all——删掉 reload 里的落点逻辑它照样绿。
// 只有预置实例再起窗口，才真正压住"已有实例 → on"这一支。
func TestGtkLandsOnClonesView(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "landing")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 预置档案与实例：这条用例只验落点，不跑探测。
	profile := core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "overlay",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}
	if _, err := env.CreateInstance(context.Background(), "demo", profile, "main"); err != nil {
		t.Fatal(err)
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkLanding",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	u := &ui{app: app, env: env, lang: "zh_CN"}
	var runErr error
	activated := false
	fail := func(format string, a ...any) {
		if runErr == nil {
			runErr = fmt.Errorf(format, a...)
		}
		app.Quit()
	}
	// 落点只决定一次：首轮落地后切到「所有应用」，再触发一轮 reload，
	// 视图必须留在用户选的地方（把 reload 里的 first 守卫删掉，这条会红）。
	reloadedAt := time.Time{}
	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		activated = true
		u.buildWindow()
		deadline := time.Now().Add(20 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				fail("等首轮扫描超时")
				return false
			}
			if !u.loaded {
				return true
			}
			if !u.stack.GetMapped() {
				return true // 等窗口映射；一直不映射由总超时判红
			}
			if reloadedAt.IsZero() {
				if got := u.stack.GetVisibleChildName(); got != "on" {
					fail("已有实例时首屏应落在「分身」，实际 %q", got)
					return false
				}
				if !u.onPage.GetVisible() {
					fail("有实例时「分身」视图的列表页不可见")
					return false
				}
				if u.emptyPage.GetVisible() {
					fail("有实例时不该显示空态")
					return false
				}
				u.stack.SetVisibleChildName("all")
				reloadedAt = time.Now()
				u.reload()
				return true
			}
			// 给第二轮扫描留出落地与观察窗口（本地只有几个 .desktop）。
			if time.Since(reloadedAt) < 1500*time.Millisecond {
				return true
			}
			if got := u.stack.GetVisibleChildName(); got != "all" {
				fail("首轮之后用户切走的视图被 reload 弹回 %q（落点必须只决定一次）", got)
				return false
			}
			app.Quit()
			return false
		}
		glib.TimeoutAdd(50, &tick, 0)
	}
	app.ConnectActivate(&actcb)
	u.hold(&actcb)

	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK 主循环退出码 %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !activated {
		t.Fatal("activate 没有执行（GApplication 把调用转发给了别的实例？）")
	}
}

// TestGtkEmptyStateMapped：切到「分身」视图后，空态必须真的被映射（用户看得到）。
//
// 只断言 GetVisible/GetParent 是假绿：visible 是局部属性，祖先隐藏时仍为真；
// parent 也只是"有爹"。这里切到 on 页，等一帧后断言 GetMapped，并且父容器
// 必须就是 onBox——把 StatusPage 错挂到 offBox 或让 onPage 同时可见都会红。
func TestGtkEmptyStateMapped(t *testing.T) {
	env, _ := gtkTestEnv(t, "empty")

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkEmpty",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	u := &ui{app: app, env: env, lang: "zh_CN"}
	var runErr error
	activated := false
	fail := func(format string, a ...any) {
		if runErr == nil {
			runErr = fmt.Errorf(format, a...)
		}
		app.Quit()
	}
	switched := false
	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		activated = true
		u.buildWindow()
		deadline := time.Now().Add(20 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				fail("等首轮扫描/映射超时")
				return false
			}
			if !u.loaded || !u.stack.GetMapped() {
				return true
			}
			if !switched {
				if got := u.stack.GetVisibleChildName(); got != "all" {
					fail("没有分身时首屏应落在「所有应用」，实际 %q", got)
					return false
				}
				if !u.switcher.GetSensitive() {
					fail("首轮落地后视图切换器仍被禁用")
					return false
				}
				u.stack.SetVisibleChildName("on")
				switched = true
				return true
			}
			// 切页后等一帧，让映射状态更新。
			if !u.emptyPage.GetMapped() {
				return true
			}
			if p := u.emptyPage.GetParent(); p == nil {
				fail("空态页没有父容器")
				return false
			} else {
				same := p.GoPointer() == u.onBox.Widget.GoPointer()
				p.Unref()
				if !same {
					fail("空态页挂在了「分身」视图之外")
					return false
				}
			}
			if u.onPage.GetVisible() {
				fail("空态时「分身」视图的列表页不该可见")
				return false
			}
			app.Quit()
			return false
		}
		glib.TimeoutAdd(50, &tick, 0)
	}
	app.ConnectActivate(&actcb)
	u.hold(&actcb)

	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK 主循环退出码 %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !activated {
		t.Fatal("activate 没有执行（GApplication 把调用转发给了别的实例？）")
	}
}

// findListRow 在一组行里按标题找控件。标题是产品自己 SetTitle 写进去的，
// 读它等于问"用户看到的这一行是不是它"。返回的包装带引用，调用方负责 Unref。
func findListRow(g *adw.PreferencesGroup, title string) *gtk.Widget {
	for i := uint32(0); ; i++ {
		w := g.GetRow(i)
		if w == nil {
			return nil
		}
		if stringProp(w, "title") == title {
			return w
		}
		w.Unref()
	}
}

// stringProp 读一个字符串属性（GetProperty 需要先按类型初始化 Value）。
func stringProp(w *gtk.Widget, name string) string {
	var v gobject.Value
	v.Init(gobject.TypeStringVal)
	w.GetProperty(name, &v)
	s := v.GetString()
	v.Unset()
	return s
}

// setBool 通过 gobject 属性接口改值：这正是 gtk_switch_set_active 走的路，
// notify::active 会照常发出，产品登记的回调因此被触发。
func setBool(w *gtk.Widget, name string, val bool) {
	var v gobject.Value
	v.Init(gobject.TypeBooleanVal)
	v.SetBoolean(val)
	w.SetProperty(name, &v)
	v.Unset()
}

// visibleIsPage 判断某个导航页是否是屏幕上那一页。GetVisiblePage 是
// transfer full（puregotk 已 IncreaseRef），查完必须 Unref。
func visibleIsPage(u *ui, page *adw.NavigationPage) bool {
	vis := u.nav.GetVisiblePage()
	if vis == nil {
		return false
	}
	ok := vis.GoPointer() == page.GoPointer()
	vis.Unref()
	return ok
}
