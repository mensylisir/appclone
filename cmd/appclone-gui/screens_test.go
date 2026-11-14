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
// 这些用例走产品回调而不是合成点击：拨 SwitchRow 的 active 会发真实
// notify::active 进 u.onSwitch；确认页直调的是按钮 clicked 绑定的同一个
// submit 处理函数。真实指针/键盘事件由同目录的 input_uinput_test.go 单独覆盖
// （uinput，Wayland 原生，不需要 XTEST/broadway）。
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

// noFuseMounter：闸门只验页面生命周期，不该在 $TMPDIR 做一次真实
// fuse-overlay 冒烟挂载（checkEnv 会调 Available）。报**后端缺失**这一
// 精确原因（真实实现缺二进制时也是这个键），界面才会走 strace 降级分支；
// 随便一个 error 现在按"挂载被拒"处理（ADR-032），不会降级。
// 降级判定还要求 PATH 里有 strace——闸门环境（本机与 CI）都装。
type noFuseMounter struct{}

func (noFuseMounter) Available() error {
	return &core.Error{Code: core.CodeEnv, Key: "err.probe.no_fuse_overlayfs"}
}

func (noFuseMounter) Mount(string, string, string, string) (func() error, error) {
	return nil, errors.New("gtk e2e：不做挂载")
}

// deniedMounter：fuse 装了、但挂载被拒（权限/沙盒）。这是 ADR-032 明确
// 不许静默降级的那一类——界面必须报错并给出环境处置，不能显示"兼容模式"。
type deniedMounter struct{}

func (deniedMounter) Available() error {
	return &core.Error{Code: core.CodeEnv, Key: "err.probe.mount_failed"}
}

func (deniedMounter) Mount(string, string, string, string) (func() error, error) {
	return nil, errors.New("gtk e2e：不做挂载")
}

// availableMounter：自检报告 fuse 可用、但从不真的挂载。用于"开关必须
// 可拨"的用例——noFuseMounter 会让界面进入降级态并按设计禁用克隆开关，
// 用例再去拨它就成了和产品规则对抗（自检晚到的 SetSensitive 还会造成偶发
// 回弹）。降级禁用由 TestGtkForkDisabledWhenFuseUnavailable 单独覆盖。
type availableMounter struct{}

func (availableMounter) Available() error { return nil }

func (availableMounter) Mount(string, string, string, string) (func() error, error) {
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
	// 探测写层根默认是 /tmp；钉到测试根下，取消探测留下的临时目录
	// 也只落在 $WORK，而不是共享临时目录。
	env.TmpRoot = filepath.Join(root, "tmp")
	if err := os.MkdirAll(env.TmpRoot, 0o700); err != nil {
		t.Fatal(err)
	}
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
		// Esc 的页面导航语义只有一个处理者：nav 自带的 pop-on-escape 是结构性
		// 不变量，任何路径都不得重新打开；否则焦点在子控件时它会先于窗口控制器
		// 冒泡触发，把"探测页取消"变成"返回"。对话框与搜索框的 Esc 不受影响。
		if u.nav.GetPopOnEscape() {
			fail("AdwNavigationView 的 pop-on-escape 必须关闭")
			return
		}
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
				// 默认窗口尺寸：三屏内容（列表 + 详情）是按宽窗口排的，退回
				// 窄竖条会让行内动作挤成两行。回归只钉下限，不钉具体像素。
				var dw, dh int32
				u.win.GetDefaultSize(&dw, &dh)
				if dw < 800 || dh < 600 {
					fail("窗口默认尺寸过窄：%dx%d（应至少 800x600）", dw, dh)
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
					// 发真实 clicked 信号：直接调 v.submit() 会在「忘把按钮
					// 接到处理器」时仍然变绿（界面点了没反应）。
					gobject.SignalEmitByName(&v.confirmBtn.Widget.Object, "clicked")
					// creating 是事务态（§6.1 ③-c）：按钮全部停用、页面不可
					// 返回。只在 steps 里路过而不断言，会让这些约束悄悄回归。
					if v.state != "creating" {
						fail("确认后没有进入 creating 态（state=%q）", v.state)
						return false
					}
					if v.confirmBtn.GetSensitive() || v.cancelBtn.GetSensitive() {
						fail("creating 期间确认/取消按钮仍可点")
						return false
					}
					if v.page.GetCanPop() {
						fail("creating 期间页面仍可返回（SetCanPop(false) 丢了？）")
						return false
					}
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
				// 等详情页真的是可见页再返回：断言必须落在已映射的页上，
				// 而不是一张还在 push 过渡里的页。
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

// TestGtkSetupStates 覆盖配置页的「探测中」态与列表页降级横幅的显现/「了解」消失。
//
// 为什么需要它：生命周期用例走的是 open 入口（不探测），只证明了 confirm →
// creating 两态；探测中没有自动断言，而 ADR-027 的退场条件是「闸门走遍三屏四态」。
// 这里用产品自己的入口构造：拨开关进入首次配置 → start() 同步把栈切到 probing；
// 直接 finish(false)（取消按钮绑的就是它）→ 页面 pop 且不留实例/档案。
//
// 取消发生在 Probe 的会话真正写盘之前（页面一上屏就点「取消探测」）。后台
// 探测可能已经跑了 LoadApp/锁/StaticHints 几步，断言的是它的结果没有提交：
// 不落档案、不建实例。这正是本机没有 XTEST 注入时仍能走通的真实路径。
func TestGtkSetupStates(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "states")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkStates",
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
	step := 0
	var v *setupView
	probeClosed := false
	bannerDismissed := false

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
				fail("配置页状态机没有在 20 秒内推进（停在第 %d 步）", step)
				return false
			}
			switch step {
			case 0:
				if !u.loaded || !u.envChecked || !u.stack.GetMapped() {
					return true
				}
				// noFuseMounter 让自检走降级分支：横幅必须真的挂出来，
				// 而不是只置了内部标志。
				if !u.softBanner.GetRevealed() {
					fail("缺 fuse 时降级横幅没有显示")
					return false
				}
				if !u.search.GetSensitive() {
					fail("降级（非致命）时搜索不该被禁用")
					return false
				}
				// 「了解」是用户处理降级提示的唯一动作。这里发真实的
				// button-clicked 信号，走产品 ConnectButtonClicked 的接线——
				// 直接调 softDismissFn 会在"忘连接信号"时仍然变绿。
				gobject.SignalEmitByName(&u.softBanner.Widget.Object, "button-clicked")
				bannerDismissed = u.softDismissed && !u.softBanner.GetRevealed()
				if !bannerDismissed {
					fail("点「了解」后降级横幅仍然显示（按钮信号没有接到处理器？）")
					return false
				}
				u.applyEnvBanners()
				if u.softBanner.GetRevealed() {
					fail("降级横幅被后续刷新弹回来了（softDismissed 失效）")
					return false
				}
				if !u.pushSetup("demo", "setup", func(bool) { probeClosed = true }) {
					fail("首次配置页没有推出来")
					return false
				}
				v = u.activeSetup
				if v == nil {
					fail("pushSetup 后 activeSetup 为空")
					return false
				}
				// probing 是同步置上的：start() 在推页返回前就 setState("probing")。
				if v.state != "probing" || v.stack.GetVisibleChildName() != "probing" {
					fail("首次配置没有进入探测态（state=%q 可见页=%q）", v.state, v.stack.GetVisibleChildName())
					return false
				}
				if v.confirmBtn.GetVisible() {
					fail("探测中不该显示确认按钮")
					return false
				}
				// Long English live feedback must wrap in a narrow native page.
				u.lang = "en"
				v.applyProbeProgress(v.probeSeq, core.ProbeProgress{Stage: "round_running",
					Round: 1, Rounds: 2, ReadAvailable: true, Reads: 100000})
				var minimum, natural, minBaseline, natBaseline int32
				v.countLabel.Measure(gtk.OrientationHorizontalValue, -1, &minimum, &natural, &minBaseline, &natBaseline)
				u.lang = "zh_CN"
				if minimum > 300 {
					fail("读取进度撑宽页面：最小宽度 %d", minimum)
					return false
				}
				if v.cancelBtn.GetLabel() != "取消探测" {
					fail("探测中取消按钮应叫「取消探测」，实际 %q", v.cancelBtn.GetLabel())
					return false
				}
				step++
			case 1:
				// 等页面真的上屏再取消：按钮信号必须作用在已映射的页上。
				// （早期版本把 20s 超时归因于"push 中 Pop 被忽略"，实测
				// libadwaita 1.10 里 Pop 会同步生效；等待是为了断言的确定性。）
				if !visibleIsPage(u, v.page) {
					return true
				}
				// 发真实 clicked 信号，验证取消按钮的接线而不只是函数地址。
				gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
				step++
			case 2:
				// v.held 只有在探测 goroutine 真正返回、release 之后才为假：
				// 它的 defer 负责杀 dbus-daemon 与 strace 进程组。不等它，
				// 测试进程会在 terminateGroup 的 3 秒窗口里退出，留下一组
				// 孤儿进程（还会攥着 go test 的 stderr 管道，导致闸门偶发
				// 「Test I/O incomplete」）。
				if u.activeSetup != nil || !probeClosed || v.held {
					return true // 等 pop 收尾与探测清理
				}
				insts, err := u.env.ListInstances("demo")
				if err != nil || len(insts) != 0 {
					fail("取消探测后不该留下实例（%d 个，%v）", len(insts), err)
					return false
				}
				if _, err := u.env.LoadProfile("demo"); err == nil {
					fail("取消探测后不该落档案")
					return false
				}
				if u.fatalBanner.GetRevealed() {
					fail("bwrap 可用时不该显示致命横幅")
					return false
				}
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
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !activated || step != 2 {
		t.Fatalf("配置页状态用例没有跑完：activated=%t step=%d", activated, step)
	}
	if !bannerDismissed {
		t.Fatal("降级横幅的「了解」路径没有执行")
	}
}

// TestGtkErrorState 覆盖配置页的「失败」态：版本超前的档案在 open 入口
// 被拒绝后，页面必须进入 error 态，且不可重试的失败不显示「重试」。
//
// 单独成测而不是接在 TestGtkSetupStates 后面：同一个导航视图里 pop 完
// 立刻 push 同名 tag 的页会被 AdwNavigationView 拒绝（过渡态），靠 sleep
// 等动画既慢又有负载下的时序窗口。独立窗口没有过渡态，push 后等
// visibleIsPage 为真再断言，杜绝"断言在一个没上屏的页上"的假绿。
func TestGtkErrorState(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "errorstate")
	if err := os.WriteFile(filepath.Join(appsDir, "bad.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Bad\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 版本超前：LoadProfile 必须拒绝，且这不是重试能解决的问题。
	if err := env.SaveProfile(core.Profile{
		App: "bad", Version: core.ProfileVersion + 1, Exec: "/usr/bin/true",
		ProbeMethod: "overlay", Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkError",
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
	closed := false
	closedOK := true // 取消必须回 onClose(false)，不能把一次取消报成成功
	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		activated = true
		u.buildWindow()
		deadline := time.Now().Add(20 * time.Second)
		var v *setupView
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				fail("失败态用例没有在 20 秒内推进")
				return false
			}
			if v == nil {
				if !u.loaded {
					return true
				}
				if !u.pushSetup("bad", "open", func(ok bool) { closed = true; closedOK = ok }) {
					fail("失败态用例的配置页没有推出来")
					return false
				}
				v = u.activeSetup
				if v == nil {
					fail("推页后 activeSetup 为空")
					return false
				}
				return true
			}
			if closed {
				app.Quit()
				return false
			}
			if !visibleIsPage(u, v.page) {
				return true // 等 push 动画；一直不可见由总超时判红
			}
			if v.state != "error" || v.stack.GetVisibleChildName() != "error" {
				fail("版本超前的档案没有进入失败态（state=%q）", v.state)
				return false
			}
			if got := v.errPage.GetTitle(); got != "档案由更新版本生成" {
				fail("失败态标题没有按原因分类，得到 %q", got)
				return false
			}
			if v.errPage.GetDescription() == "" {
				fail("失败态没有说明下一步（描述为空）")
				return false
			}
			if v.retry != nil {
				fail("版本超前是可重试路径之外的情况，retry 必须为 nil")
				return false
			}
			if v.confirmBtn.GetVisible() {
				fail("不可重试的失败态不该显示重试按钮")
				return false
			}
			if v.cancelBtn.GetLabel() != "取消" {
				fail("失败态取消按钮文案应为「取消」，实际 %q", v.cancelBtn.GetLabel())
				return false
			}
			gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
			return true
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
	if !activated || !closed {
		t.Fatalf("失败态用例没有跑完：activated=%t closed=%t", activated, closed)
	}
	if closedOK {
		t.Fatal("取消失败态必须回 onClose(false)，实际报告成功")
	}
}

// TestGtkRetryableError：可重试的失败态必须显示「重试」，点下去要重跑
// 失败的那件事（§6.1 ③-d）。
//
// 真实探测失败不是随时能造的（要应用在跑/零写入/锁被占），所以这里直接
// 构造失败态的两条不可见约束：retry != nil → 按钮可见可点；点击 → 调用
// retry 而不是 submit 的确认分支。
func TestGtkRetryableError(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "retryable")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.SaveProfile(core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "overlay",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkRetry",
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
	retried := false
	closed := false
	step := 0
	var v *setupView
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
				fail("可重试失败态用例没有在 20 秒内推进（停在第 %d 步）", step)
				return false
			}
			switch step {
			case 0:
				if !u.loaded {
					return true
				}
				// open 入口复用档案，直接进确认态，不跑探测。
				u.pushSetup("demo", "open", func(bool) { closed = true })
				v = u.activeSetup
				if v == nil || v.state != "confirm" {
					fail("open 入口没有进入确认态")
					return false
				}
				step++
			case 1:
				if !visibleIsPage(u, v.page) {
					return true
				}
				// 模拟探测失败后的失败态：retry 指向"失败的那件事"。
				v.retry = func() { retried = true }
				v.showErrorTitle(v.u.tr("ui.setup.error.generic", nil), errors.New("boom"))
				if v.state != "error" {
					fail("没有进入失败态：%q", v.state)
					return false
				}
				if !v.confirmBtn.GetVisible() || !v.confirmBtn.GetSensitive() {
					fail("可重试的失败态必须显示可点的「重试」")
					return false
				}
				if got := v.confirmBtn.GetLabel(); got != "重试" {
					fail("失败态按钮应叫「重试」，实际 %q", got)
					return false
				}
				gobject.SignalEmitByName(&v.confirmBtn.Widget.Object, "clicked")
				if !retried {
					fail("点「重试」没有调用 retry（走错到确认分支了？）")
					return false
				}
				gobject.SignalEmitByName(&v.cancelBtn.Widget.Object, "clicked")
				step++
			case 2:
				if !closed {
					return true
				}
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
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !activated || !closed || !retried {
		t.Fatalf("可重试失败态用例没有跑完：activated=%t closed=%t retried=%t", activated, closed, retried)
	}
}

// TestGtkRapidRepush：取消配置页后立刻再拨一次开关，第二页必须真的上屏。
//
// 这是 P1（对抗性复审）：AdwNavigationView 在 pop 过渡期间仍持有旧页 tag，
// 旧实现用固定 tag "setup"，紧接着 Push 同名页会被静默拒绝——activeSetup
// 指向一个没上屏的页面，开关/详情从此点不开。现在 pushPage 给每页唯一
// tag，这条用例必须能走通；把 tag 改回固定值会立即变红。
func TestGtkRapidRepush(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "repush")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkRepush",
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
	closed := 0
	step := 0
	var first, second *setupView
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
				fail("快速重推用例没有在 20 秒内推进（停在第 %d 步）", step)
				return false
			}
			switch step {
			case 0:
				if !u.loaded {
					return true
				}
				u.pushSetup("demo", "setup", func(bool) { closed++ })
				first = u.activeSetup
				if first == nil {
					fail("第一页没有推出来")
					return false
				}
				step++
			case 1:
				if !visibleIsPage(u, first.page) {
					return true
				}
				// 取消的同一拍立刻再推一页：真实用户就是这样连点的。
				gobject.SignalEmitByName(&first.cancelBtn.Widget.Object, "clicked")
				if !u.pushSetup("demo", "setup", func(bool) { closed++ }) {
					fail("取消后立刻重推被拒绝了")
					return false
				}
				second = u.activeSetup
				if second == nil {
					fail("重推后 activeSetup 为空")
					return false
				}
				if second == first {
					fail("重推复用了正在退场的旧页面")
					return false
				}
				step++
			case 2:
				// 第二页必须真的成为可见页；旧实现（固定 tag）在这里永远等不到。
				// 探测完成得快时，可见的瞬间可能已是 confirm/error（假应用不写盘
				// 就是 error），所以只要求它进入某个真实构建出来的状态。
				if !visibleIsPage(u, second.page) {
					return true
				}
				switch second.state {
				case "probing", "confirm", "error":
				default:
					fail("重推的页面没有进入有效状态：%q", second.state)
					return false
				}
				gobject.SignalEmitByName(&second.cancelBtn.Widget.Object, "clicked")
				step++
			case 3:
				// 同样要等两条探测 goroutine 都收完（held 为假）：否则测试
				// 进程先退出，strace/dbus-daemon 会变成孤儿，并攥着 go test
				// 的输出管道让闸门偶发 I/O incomplete。
				if closed < 2 || first.held || second.held {
					return true
				}
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
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !activated || closed != 2 {
		t.Fatalf("快速重推用例没有跑完：activated=%t closed=%d", activated, closed)
	}
}

// TestGtkFatalBanner：bwrap 缺失是致命态——横幅、搜索与开关必须一起停用，
// 不留下"看起来能操作"的控件。
//
// 用空 PATH 模拟：CheckBwrap 走 exec.LookPath。窗口构建前设置，进程内
// 没有别的外部命令依赖，GTK 已由 puregotk 在 init 阶段加载完毕。
func TestGtkFatalBanner(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "fatal")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkFatal",
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
				fail("等环境自检超时")
				return false
			}
			if !u.envChecked || !u.loaded {
				return true
			}
			if !u.fatal {
				fail("PATH 里没有 bwrap，自检却没有判成致命")
				return false
			}
			if !u.fatalBanner.GetRevealed() {
				fail("致命态横幅没有显示")
				return false
			}
			if u.softBanner.GetRevealed() {
				fail("致命态不该同时显示降级横幅")
				return false
			}
			if u.search.GetSensitive() {
				fail("致命态下搜索仍可操作")
				return false
			}
			// 「所有应用」组的开关必须停用：致命态下点它只会报同一条错。
			if len(u.groups) == 0 {
				fail("致命态下应用列表没有渲染")
				return false
			}
			row := findListRow(u.groups[0], "Demo")
			if row == nil {
				fail("致命态下找不到 Demo 行")
				return false
			}
			sensitive := row.GetSensitive()
			row.Unref()
			if sensitive {
				fail("致命态下应用开关仍可操作")
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

// TestGtkEnglishStrings：语言跟随 i18n.Lang() 之后，en 界面必须真的拿到
// 英文文案，而不是回落中文、更不能露出 ui.* 键名。
//
// 这是 ADR-027 退场的最后一道保险：消息表由 TestTablesMatch 保证键齐全，
// 这里保证 GUI 真的按 lang 查表（漏改某个字面量、把 lang 写死成 zh_CN，
// 都会让这条变红）。
func TestGtkEnglishStrings(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "english")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkEnglish",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	// 走产品自己的构造入口：语言由 i18n.Lang() 按 LC_ALL 判定，避免
	// 测试自己写 lang 而漏掉 main → newUI 的接线。
	t.Setenv("LC_ALL", "en_US.UTF-8")
	u := newUI(app, env)
	if u.lang != "en" {
		t.Fatalf("LC_ALL=en_US.UTF-8 时界面语言应为 en，得到 %q", u.lang)
	}
	var runErr error
	activated := false
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
			if got := u.win.GetTitle(); got != "App Clones" {
				fail("en 界面窗口标题应为 \"App Clones\"，得到 %q", got)
				return false
			}
			if got := u.emptyPage.GetTitle(); got != "No clones yet" {
				fail("en 界面空态标题不对：%q", got)
				return false
			}
			// 用字面量前缀而不是查表：查表比对会把"消息表本身写错"也放过。
			if got := u.softBanner.GetTitle(); !strings.HasPrefix(got, "Probing will run in compatible mode") {
				fail("en 界面降级横幅不是英文文案：%q", got)
				return false
			}
			// 缺键的界面表现是原始键名（ui.xxx）——这里一并堵住。
			for _, text := range []string{u.win.GetTitle(), u.emptyPage.GetTitle(), u.softBanner.GetTitle()} {
				if strings.Contains(text, "ui.") {
					fail("界面露出了消息键名：%q", text)
					return false
				}
			}
			if len(u.groups) == 0 {
				fail("应用列表没有渲染")
				return false
			}
			row := findListRow(u.groups[0], "Demo")
			if row == nil {
				fail("找不到 Demo 行")
				return false
			}
			sub := stringProp(row, "subtitle")
			row.Unref()
			if sub != "No isolation profile yet" {
				fail("en 界面应用副标题不对：%q", sub)
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

// typeNameOf 返回 GObject 类型名（如 AdwActionRow）。puregotk 没有暴露
// GetType 包装，只能按 GTypeInstance 布局读；GoPointer 是 C 对象地址，
// 先落到 unsafe.Pointer 再转（直接 uintptr→unsafe.Pointer 会被 vet 拦）。
func typeNameOf(w *gtk.Widget) string {
	p := w.GoPointer()
	return gobject.TypeNameFromInstance((*gobject.TypeInstance)(*(*unsafe.Pointer)(unsafe.Pointer(&p))))
}

// findListRow 在一组行里按标题找控件。标题是产品自己 SetTitle 写进去的，
// 读它等于问"用户看到的这一行是不是它"。返回的包装带引用，调用方负责 Unref。
//
// 不用 `adw.PreferencesGroup.GetRow`：`adw_preferences_group_get_row` 在
// libadwaita 1.5（ubuntu-24.04，CI 的 GTK job）不存在，puregotk 注册出的
// 函数指针为 nil，一调用就 panic（2026-11-08 在 ubuntu:24.04 容器里实测）。
// 改为在控件树里找 GtkListBox，它的直接子节点就是行。
func findListRow(g *adw.PreferencesGroup, title string) *gtk.Widget {
	boxes := findWidgets(&g.Widget, func(w *gtk.Widget) bool {
		return typeNameOf(w) == "GtkListBox"
	})
	if len(boxes) == 0 {
		return nil
	}
	for c := boxes[0].GetFirstChild(); c != nil; c = c.GetNextSibling() {
		if stringProp(c, "title") == title {
			// GetFirstChild 的包装已 IncreaseRef，与旧 GetRow 的语义相同；
			// 不要再 Ref，调用方的 Unref 正好抵消这一份。
			return c
		}
	}
	return nil
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

// findWidgets 深度优先遍历控件树。按钮没有稳定 id，回归靠 tooltip 认出
// "启动/停止"——tooltip 是产品给用户看的文案，改了它本来就该触发断言。
// GetFirstChild/GetNextSibling 都是 transfer none，不 Unref。
func findWidgets(root *gtk.Widget, pred func(*gtk.Widget) bool) []*gtk.Widget {
	var out []*gtk.Widget
	var walk func(*gtk.Widget)
	walk = func(w *gtk.Widget) {
		if w == nil {
			return
		}
		if pred(w) {
			out = append(out, w)
		}
		for c := w.GetFirstChild(); c != nil; c = c.GetNextSibling() {
			walk(c)
		}
	}
	walk(root)
	return out
}

// TestGtkDetailShowsStopWhenRunning（ADR-028）：运行中的实例行必须显示
// 「停止」而不是禁用「启动」。去掉运行态分支（永远显示启动）会让这条
// 用例立即变红。
func TestGtkDetailShowsStopWhenRunning(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "stopbtn")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "overlay",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}
	if _, err := env.CreateInstance(context.Background(), "demo", profile, "main"); err != nil {
		t.Fatal(err)
	}
	// 让 IsRunning 看到"在跑"：flock 对同进程的不同 fd 也会冲突。
	lock, err := env.HoldRunLock("demo", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkStop",
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
	pushed := false
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
				fail("等详情页运行态超时")
				return false
			}
			if !u.loaded {
				return true
			}
			if !pushed {
				pushed = true
				u.pushDetail("demo")
				return true
			}
			v := u.activeDetail
			if v == nil || !v.loaded {
				return true
			}
			stops := findWidgets(&v.content.Widget, func(w *gtk.Widget) bool {
				return w.GetTooltipText() == u.tr("ui.instance.stop.tooltip", nil)
			})
			if len(stops) != 1 {
				fail("运行中的实例行应恰好有一个「停止」按钮，得到 %d", len(stops))
				return false
			}
			if !stops[0].GetSensitive() {
				fail("「停止」按钮不可点")
				return false
			}
			starts := findWidgets(&v.content.Widget, func(w *gtk.Widget) bool {
				return w.GetTooltipText() == u.tr("ui.instance.launch.tooltip", nil)
			})
			if len(starts) != 0 {
				fail("运行中不应同时显示「启动」按钮，得到 %d", len(starts))
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
		t.Fatal("activate 没有执行")
	}
}

// TestGtkForkTogglePersists（§8.1/ADR-029）：确认页的数据来源开关默认关，
// 打开后建出的实例必须把 fork 写进 instance.toml——开关只在界面上亮着、
// 没进 CreateOptions 的话，用户以为克隆了、实际拿到空分身。
func TestGtkForkTogglePersists(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "fork")
	// 降级态按设计禁用克隆开关；本用例验的是开关接线，给一个"fuse 可用"的
	// 自检结果（不会真的挂载）。
	env.Mounter = availableMounter{}
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "manual",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}
	if err := env.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := env.LoadProfile("demo"); err != nil {
		t.Fatalf("测试预置档案读不回来：%v", err)
	}
	// 建实例要生成 .desktop 的 Exec，找不到 CLI 会报 create 失败。
	env.CliPath = "/usr/bin/appclone"

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkFork",
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
	opened, submitted := false, false
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
				state, desc := "<nil>", ""
				if v := u.activeSetup; v != nil {
					state = v.state
					if v.errPage != nil {
						desc = v.errPage.GetTitle() + " / " + v.errPage.GetDescription()
					}
				}
				fail("等克隆起步开关/实例落盘超时（loaded=%v setupState=%s desc=%q）", u.loaded, state, desc)
				return false
			}
			if !u.loaded {
				return true
			}
			if !opened {
				opened = true
				// open 入口复用档案、跳过探测，直接进确认页。
				if !u.pushSetup("demo", "open", func(bool) {}) {
					fail("pushSetup 被拒绝")
					return false
				}
				return true
			}
			v := u.activeSetup
			if !submitted {
				if v == nil || v.state != "confirm" {
					return true
				}
				if v.forkRow == nil {
					fail("建实例的确认页必须显示数据来源开关")
					return false
				}
				// 行必须真在控件树上：只建对象不 Add，SetActive 照样能改值、
				// instance.toml 也照样写 fork=true，用户却看不到这个开关。
				if p := v.forkRow.GetParent(); p == nil {
					fail("数据来源开关没有挂进确认页（group.Add 漏了？）")
					return false
				} else {
					p.Unref()
				}
				if v.forkRow.GetActive() {
					fail("克隆起步默认应为关（与 CLI 默认一致）")
					return false
				}
				v.forkRow.SetActive(true)
				if !v.fork {
					fail("开关 notify 没有接到 v.fork")
					return false
				}
				// rediscover 只更新档案、不建实例：同一份确认页按这个 mode 重渲染
				// 时开关必须消失（否则用户拨了它，最终什么都没发生）。
				v.mode = "rediscover"
				v.renderConfirm()
				if v.forkRow != nil {
					fail("rediscover 确认页不该出现数据来源开关")
					return false
				}
				v.mode = "open"
				v.renderConfirm()
				if v.forkRow == nil || !v.forkRow.GetActive() {
					fail("回到建实例入口后，数据来源开关应恢复且保持已勾选")
					return false
				}
				v.submit()
				submitted = true
				return true
			}
			in, err := env.GetInstance("demo", "main")
			if err != nil {
				return true // 事务还没落盘
			}
			if !in.Fork {
				fail("开关打开了，但 instance.toml 没有 fork=true")
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
		t.Fatal("activate 没有执行")
	}
}

// TestGtkForkDisabledWhenFuseUnavailable：fuse 不可用（后端缺失走 strace 降级，
// 或挂载被拒）时「数据来源」开关必须
// 禁用。允许拨动等于让用户相信克隆已生效，而 run 期每个路径都会降级跳过，
// 最终得到一个空实例。判据是自检结果（noFuseMounter → degraded），不是渲染
// 时刻的巧合；用例等 envChecked 再判定。
func TestGtkForkDisabledWhenFuseUnavailable(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "forkdegraded")
	env.Mounter = noFuseMounter{}
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.SaveProfile(core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "manual",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	env.CliPath = "/usr/bin/appclone"

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkForkDegraded",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	u := &ui{app: app, env: env, lang: "zh_CN"}
	var runErr error
	activated, opened := false, false
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
		deadline := time.Now().Add(20 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				fail("等待降级自检/确认页超时（loaded=%v envChecked=%v）", u.loaded, u.envChecked)
				return false
			}
			if !u.loaded || !u.envChecked {
				return true
			}
			if !u.degraded || !u.fuseBlocked {
				fail("后端缺失且 PATH 有 strace 时应判成降级（degraded=%v blocked=%v）", u.degraded, u.fuseBlocked)
				return false
			}
			if !opened {
				opened = true
				if !u.pushSetup("demo", "open", func(bool) {}) {
					fail("pushSetup 被拒绝")
					return false
				}
				return true
			}
			v := u.activeSetup
			if v == nil || v.state != "confirm" {
				return true
			}
			if v.forkRow == nil {
				fail("确认页没有数据来源开关")
				return false
			}
			if v.forkRow.GetSensitive() {
				fail("缺 fuse-overlayfs（降级态）时克隆起步开关必须禁用")
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
		t.Fatal("activate 没有执行")
	}
}

// TestGtkFuseDeniedIsAnErrorNotADegrade：fuse 装了但挂载被拒（权限/沙盒）
// 时，界面必须报"不可用 + 怎么处理"，而不是显示"兼容模式"。静默降级会让
// 用户在不知情下拿到低覆盖率结果——ADR-032 的核心禁止项。
func TestGtkFuseDeniedIsAnErrorNotADegrade(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "fusedenied")
	env.Mounter = deniedMounter{}
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.SaveProfile(core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "manual",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	env.CliPath = "/usr/bin/appclone"

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkFuseDenied",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	u := &ui{app: app, env: env, lang: "zh_CN"}
	var runErr error
	activated, opened := false, false
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
		deadline := time.Now().Add(20 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				fail("等环境自检/确认页超时（loaded=%v envChecked=%v）", u.loaded, u.envChecked)
				return false
			}
			if !u.loaded || !u.envChecked {
				return true
			}
			if u.fatal {
				fail("bwrap 可用时不该判成致命态")
				return false
			}
			if u.degraded {
				fail("挂载被拒不得判成 strace 降级（ADR-032）")
				return false
			}
			if !u.fuseBlocked {
				fail("挂载被拒没有记成 fuseBlocked")
				return false
			}
			if !u.softBanner.GetRevealed() {
				fail("挂载被拒时列表页没有报错横幅")
				return false
			}
			if !strings.Contains(u.softBanner.GetTitle(), "FUSE") {
				fail("横幅文案应说明 FUSE 挂载被拒，实际 %q", u.softBanner.GetTitle())
				return false
			}
			if !u.search.GetSensitive() {
				fail("挂载被拒只影响探测，不该禁用搜索")
				return false
			}
			if !opened {
				opened = true
				if !u.pushSetup("demo", "open", func(bool) {}) {
					fail("pushSetup 被拒绝")
					return false
				}
				return true
			}
			v := u.activeSetup
			if v == nil || v.state != "confirm" {
				return true
			}
			if v.forkRow == nil {
				fail("确认页没有数据来源开关")
				return false
			}
			if v.forkRow.GetSensitive() {
				fail("挂载被拒时克隆起步开关必须禁用（拨了也不会生效）")
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
		t.Fatal("activate 没有执行")
	}
}

// TestGtkAppIconFallback（用户反馈"无图标的应用太丑"）：.desktop 没写 Icon、
// 或写的绝对路径已不存在时，行前缀必须退回首字母头像，而不是一张空图或
// 千篇一律的通用图标。用例直接调用 appIcon（真实构造 AdwAvatar/GtkImage），
// 钉住三条判定（空值、缺失路径、未知主题名）与首字母提取。
func TestGtkAppIconFallback(t *testing.T) {
	if os.Getenv("APPCLONE_GTK_TEST") != "1" {
		t.Skip("GTK 回归需要显式开关：design/guitest-gtk.sh")
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("没有可用的显示（DISPLAY / WAYLAND_DISPLAY）")
	}
	gtk.Init()
	u := &ui{lang: "zh_CN"}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.png")
	if err := os.WriteFile(real, []byte("not-a-real-image-but-exists"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "not-there.png")
	for _, tc := range []struct {
		icon      string
		wantFallb bool
	}{
		{"", true},                     // 没写 Icon
		{missing, true},                // 绝对路径但文件没了
		{"ac-test-no-such-icon", true}, // 主题里没有这个名字
		{real, false},                  // 存在的绝对路径
	} {
		w, fallback := u.appIcon(tc.icon, "微信", 32)
		if w == nil {
			t.Fatalf("appIcon(%q) 返回了 nil 控件", tc.icon)
		}
		if fallback != tc.wantFallb {
			t.Errorf("appIcon(%q) fallback=%v，想要 %v（主题名必须过 HasIcon、路径必须存在）", tc.icon, fallback, tc.wantFallb)
		}
		w.Unref()
	}
	if _, ok := u.iconImage("ac-test-no-such-icon"); ok {
		t.Fatal("主题里不存在的图标名不该被判为可用")
	}
	for name, want := range map[string]string{
		"demo": "D", " demo": "D", "微信": "微", "7-Zip": "7", "  ": "?", "": "?",
	} {
		if got := avatarInitial(name); got != want {
			t.Errorf("avatarInitial(%q) = %q，想要 %q", name, got, want)
		}
	}
	// 空 Name 的 .desktop 也不能渲染成一条空白行：显示 desktop id。
	if got := appDisplayName(core.App{Key: "weird.app"}); got != "weird.app" {
		t.Errorf("空 Name 应回退到 key，得到 %q", got)
	}
	if got := appDisplayName(core.App{Key: "wechat", Name: "微信"}); got != "微信" {
		t.Errorf("有 Name 时不得改写：%q", got)
	}
}

// TestDetachedRunnerSpawn（ADR-028）：验证 GUI 起 runner 的方式本身——
// 标记环境变量真的传下去、进程真的在独立会话里（Setsid）、父进程会收尸。
// 把 runner 换成脚本，避免在闸门里真的启动 bwrap。
func TestDetachedRunnerSpawn(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "detach")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := "demo"
	profile := core.Profile{
		App: app, Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "manual",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}
	if err := env.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := env.CreateInstance(context.Background(), app, profile, "main"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	script := filepath.Join(dir, "runner.sh")
	body := fmt.Sprintf(`#!/bin/sh
sid=$(awk '{print $6}' /proc/$$/stat)
printf 'APP=%%s ID=%%s PID=%%s SID=%%s\n' "$APPCLONE_GUI_DETACH_APP" "$APPCLONE_GUI_DETACH_ID" "$$" "$sid" > %q
exit 7
`, marker)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	old := detachedExecutable
	detachedExecutable = func() (string, error) { return script, nil }
	defer func() { detachedExecutable = old }()

	wait, logPath, _, err := startDetached(env, app, "main")
	if err != nil {
		t.Fatalf("起 runner 失败：%v", err)
	}
	if werr := wait(); werr == nil {
		t.Fatal("脚本以 7 退出，wait 必须返回非 nil（GUI 靠这个判启动失败）")
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("runner 没有执行：%v（log=%s）", err, logPath)
	}
	got := string(b)
	for _, want := range []string{"APP=demo", "ID=main"} {
		if !strings.Contains(got, want) {
			t.Fatalf("环境标记没有传给 runner：想要 %q，得到 %q", want, got)
		}
	}
	var pid, sid int
	if _, err := fmt.Sscanf(got[strings.Index(got, "PID="):], "PID=%d", &pid); err != nil {
		t.Fatalf("解析 PID 失败：%q", got)
	}
	if _, err := fmt.Sscanf(got[strings.Index(got, "SID="):], "SID=%d", &sid); err != nil {
		t.Fatalf("解析 SID 失败：%q", got)
	}
	if pid != sid {
		t.Fatalf("runner 必须是会话首进程（Setsid）：pid=%d sid=%d", pid, sid)
	}
}

// TestDetachLogHelpers：run.log 是长跑应用的输出落点，必须既封顶又不把
// 非法字节塞进 GTK 字符串（ADR-028 对抗复审 P2-2）。
func TestDetachLogHelpers(t *testing.T) {
	// cappedWriter 截断超出部分，但必须对调用方报告"全写完了"，
	// 否则 exec 的 copier 会以 ErrShortWrite 提前失败。
	var buf bytes.Buffer
	cw := &cappedWriter{w: &buf, left: 4}
	if n, err := cw.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("超限写入应报告写完：n=%d err=%v", n, err)
	}
	if n, err := cw.Write([]byte("gh")); err != nil || n != 2 {
		t.Fatalf("用尽配额后的写入应被静默接纳：n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "abcd" {
		t.Fatalf("只应落盘前 4 字节，得到 %q", got)
	}

	p := filepath.Join(t.TempDir(), "run.log")
	body := "appclone: 第一行\n\nappclone-gui: 启动失败\x00\x01\xff\xfe end\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := lastLogLine(p, 0), "启动失败 end"; got != want {
		t.Fatalf("lastLogLine 应去前缀并清掉非法/控制字节：want %q got %q", want, got)
	}

	// 追加模式下不能把上一轮的最后一行当成这一轮的失败原因。
	old := "上次运行的旧原因\n"
	if err := os.WriteFile(p, []byte(old+"本次失败\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := lastLogLine(p, int64(len(old))), "本次失败"; got != want {
		t.Fatalf("只应看到 offset 之后的输出：want %q got %q", want, got)
	}
}

// TestDetachedTargetEnv：runner 标记只在两个变量都非空时才分流；普通
// GUI 启动必须走 GTK。拼错一个变量名就会让每次启动都变 runner。
func TestDetachedTargetEnv(t *testing.T) {
	t.Setenv(detachAppEnv, "")
	t.Setenv(detachIDEnv, "")
	if _, _, ok := detachedTarget(); ok {
		t.Fatal("没有标记时不应分流")
	}
	t.Setenv(detachAppEnv, "demo")
	if _, _, ok := detachedTarget(); ok {
		t.Fatal("只有 app 没有 id 时不应分流")
	}
	t.Setenv(detachIDEnv, "main")
	app, id, ok := detachedTarget()
	if !ok || app != "demo" || id != "main" {
		t.Fatalf("标记齐全时应分流：ok=%v app=%q id=%q", ok, app, id)
	}
}

// 复用档案的确认页必须按 ADR-034 展示读证据四态与凭据行；探测候选里的
// 公共目录只能是只读行（不能点亮确认按钮），读证据不可用时要有
// confirm-empty 提示行。
func TestGtkConfirmReuseEvidence(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "reuse-evidence")
	if err := os.WriteFile(filepath.Join(appsDir, "cred.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Cred\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := env.Resolver
	pub := filepath.Join(r.Home, "Downloads")
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "Demo")
	cred := filepath.Join(cfg, "credentials.json")
	profile := core.Profile{
		App: "cred", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		ProbeMethod: "manual", ReadTrace: core.ReadTraceUnavailable,
		Paths: []string{cfg}, CredentialPaths: []string{cred},
		Ignored: core.DefaultIgnored(),
	}
	if err := env.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkReuseEvidence",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	u := &ui{app: app, env: env, lang: "zh_CN"}
	var runErr error
	fail := func(format string, a ...any) {
		if runErr == nil {
			runErr = fmt.Errorf(format, a...)
		}
		app.Quit()
	}
	step := 0
	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		u.buildWindow()
		deadline := time.Now().Add(20 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if time.Now().After(deadline) {
				state, desc := "<nil>", ""
				if v := u.activeSetup; v != nil {
					state = v.state
					if v.errPage != nil {
						desc = v.errPage.GetTitle() + " / " + v.errPage.GetDescription()
					}
				}
				fail("复用确认页断言超时（step=%d state=%s desc=%q）", step, state, desc)
				return false
			}
			switch step {
			case 0:
				if !u.loaded {
					return true
				}
				if !u.pushSetup("cred", "open", func(bool) {}) {
					fail("凭据档案的确认页没有推出来")
					return false
				}
				step++
			case 1:
				v := u.activeSetup
				if v == nil || v.state != "confirm" {
					return true
				}
				row, ok := v.rows[0]
				if !ok {
					fail("凭据档案的配置路径没有可勾选行")
					return false
				}
				if !strings.Contains(row.GetSubtitle(), "credentials.json") {
					fail("凭据行必须标出登录态路径：%q", row.GetSubtitle())
					return false
				}
				if !v.confirmBtn.GetSensitive() {
					fail("勾选了应用自身数据后确认按钮必须可用")
					return false
				}
				if !v.banner.GetRevealed() ||
					!strings.Contains(v.banner.GetTitle(), u.tr("ui.setup.reads.unavailable", nil)) {
					fail("unavailable 档案必须显示读证据横幅：%q", v.banner.GetTitle())
					return false
				}

				// 探测候选里的公共目录：只读行 + 不计门禁 + confirm-empty 提示。
				v2 := &setupView{u: u, appKey: "cred", mode: "setup",
					onClose: func(bool) {}, rows: map[int]*adw.SwitchRow{}}
				v2.build()
				// 能力检测走真实 PATH 会让断言随机器漂移：钉死一份"可用"。
				env.KeyringCapsOverride = &core.KeyringCaps{
					Available: true, Daemon: "/usr/bin/gnome-keyring-daemon", DbusSend: "/usr/bin/dbus-send",
				}
				pres := core.ProbeResult{
					Candidates: []core.Candidate{
						{Path: pub, Class: core.ClassPublic, DisplayClass: core.ClassPublic, Why: "probe.why.public"},
						{Path: cfg, Class: core.ClassConfig, Checked: true,
							Credential: core.CredentialStrong, CredentialPath: cred},
					},
					ReadTrace:       core.ReadTraceUnavailable,
					KeyringEvidence: core.KeyringEvidenceObserved,
					Profile:         core.Profile{App: "cred", ProbeMethod: "overlay", IsolateKeyring: true},
				}
				p2, c2, pf2, ferr := env.FinalizeProbe("cred", pres, core.ProbeFinalize{AllowEmpty: true})
				if ferr != nil {
					fail("降级确认页落案失败：%v", ferr)
					return false
				}
				v2.applyFinalized(pres, p2, c2, pf2)
				if v2.state != "confirm" {
					fail("候选确认页没有进入 confirm：%s", v2.state)
					return false
				}
				if _, ok := v2.rows[1]; !ok || len(v2.rows) != 1 {
					fail("公共候选不能成为可勾选行：%d 行", len(v2.rows))
					return false
				}
				if findListRowByTitle(v2.confirmPage, u.allocText(pub)) == nil {
					fail("公共目录必须渲染成只读行（现有行：%v）", listRowTitles(v2.confirmPage))
					return false
				}
				if findListRowByTitle(v2.confirmPage, u.tr("ui.setup.confirm.empty", nil)) == nil {
					fail("读证据不可用时必须显示 confirm-empty 提示行")
					return false
				}
				// 登录状态组始终出现：探测到使用证据时默认开启，副标题点名证据。
				// 没有它，用户就只剩下手写档案这一条路（违反零门槛红线）。
				krRow := findListRowByTitle(v2.confirmPage, u.tr("ui.setup.row.keyring.title", nil))
				if krRow == nil {
					fail("确认页缺少「独立密钥环」行（现有行：%v）", listRowTitles(v2.confirmPage))
					return false
				}
				if v2.keyringRow == nil || !v2.keyringRow.GetActive() {
					fail("观测到使用证据时应默认开启独立密钥环")
					return false
				}
				if sub := stringProp(krRow, "subtitle"); !strings.Contains(sub, u.tr("ui.setup.row.keyring.observed", nil)) {
					fail("独立密钥环行必须说明证据来源：%q", sub)
					return false
				}
				v2.scope.close()
				v2.fixed.close()
				app.Quit()
				return false
			}
			return true
		}
		glib.TimeoutAdd(50, &tick, 0)
	}
	app.ConnectActivate(&actcb)
	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GApplication 退出码 %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if step != 1 {
		t.Fatalf("用例没有走完（step=%d）", step)
	}
}

func listRowTitles(page *adw.PreferencesPage) []string {
	var out []string
	for _, box := range findWidgets(&page.Widget, func(w *gtk.Widget) bool {
		return typeNameOf(w) == "GtkListBox"
	}) {
		for c := box.GetFirstChild(); c != nil; c = c.GetNextSibling() {
			out = append(out, stringProp(c, "title"))
		}
	}
	return out
}

// findListRowByTitle 在确认页里按 title 找行（SwitchRow/ActionRow 都适用）。
func findListRowByTitle(page *adw.PreferencesPage, title string) *gtk.Widget {
	for _, box := range findWidgets(&page.Widget, func(w *gtk.Widget) bool {
		return typeNameOf(w) == "GtkListBox"
	}) {
		for c := box.GetFirstChild(); c != nil; c = c.GetNextSibling() {
			if stringProp(c, "title") == title {
				return c
			}
		}
	}
	return nil
}

// 详情页"独立密钥环"行：关闭态必须区分"私有总线在跑"与"真的共用宿主总线"。
// 私有总线没有 servicedir，宿主 Secret Service 不会被激活（ADR-035 ②），
// 说"与主实例共用宿主密钥环"会与升级迁移提示自相矛盾。
func TestKeyringRowState(t *testing.T) {
	cases := []struct {
		name string
		p    core.Profile
		want string
	}{
		{"on", core.Profile{IsolateKeyring: true}, "ui.detail.keyring.on"},
		{"off-bus", core.Profile{IsolateDBus: true}, "ui.detail.keyring.off_bus"},
		{"off", core.Profile{}, "ui.detail.keyring.off"},
	}
	for _, c := range cases {
		if got := keyringRowState(c.p); got != c.want {
			t.Fatalf("%s: 期望 %s，得到 %s", c.name, c.want, got)
		}
	}
}
