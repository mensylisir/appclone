// Command appclone-gui 是普通用户的主入口：GTK4 + libadwaita 原生三屏，
// 经 puregotk 绑定（零 cgo）。core 是唯一事实来源，这里只做展示与交互。
package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gdk"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
	"github.com/mensylisir/appclone/core/i18n"
)

// 列表上限（ADR-024）：AdwPreferencesGroup 不是虚拟化列表，
// 一次建 400 行会让搜索输入明显顿挫。
const (
	listLimit   = 30
	searchLimit = 100
	// 搜索防抖：逐字符重建两组（最多 130 行）会让输入卡顿。
	searchDebounceMs = 120
)

// dialogHook 仅 GTK 闸门使用：对话框 Choose 之前把对象交给测试，让测试能
// 走真实 response 回调（本会话无法注入指针事件）。生产进程里恒为 nil。
var dialogHook func(*adw.AlertDialog)

type ui struct {
	app  *adw.Application
	env  *core.Env
	lang string

	win    *adw.ApplicationWindow
	nav    *adw.NavigationView
	toast  *adw.ToastOverlay
	header *adw.HeaderBar
	search *gtk.SearchEntry
	// 两个视图：分身（已开启）与所有应用。用户之前反馈两组挤在一个长列表
	// 里——开关都在下半部分，已开启的应用要滚过一遍才看得到。现在用
	// AdwViewSwitcher 放在标题栏，视图之间平级切换，各自独立滚动。
	stack    *adw.ViewStack
	switcher *adw.ViewSwitcher
	// onPageObj 是「分身」视图的 ViewStackPage，用来更新标题栏上的数量角标。
	onPageObj *adw.ViewStackPage
	onBox     *gtk.Box
	offBox    *gtk.Box
	onPage    *adw.PreferencesPage
	offPage   *adw.PreferencesPage
	// emptyPage 是「分身」视图的空态。它不能塞进 AdwPreferencesGroup——
	// Group 只接受 AdwPreferencesRow，StatusPage 放进去会被裁成半截图标
	// （截图上只剩一个灰色半圆）。它直接挂在 onBox 里，随实例数显隐。
	emptyPage *adw.StatusPage

	// 环境自检（§4.5）：bwrap 缺失 = 致命（整页只读），缺 fuse = 降级
	// （列表页横幅可「了解」收掉，探测页那条不可收）。
	fatalBanner *adw.Banner
	softBanner  *adw.Banner
	// softDismissFn 是 banner 按钮的唯一回调变量：地址恒定，只连一次。
	softDismissFn func(adw.Banner)
	fatal         bool
	degraded      bool
	// fuseBlocked：fuse 挂载不可用（缺失或权限/沙盒被拒）。与 degraded 分开：
	// 只有前者能自动降级 strace（ADR-032）；但两者都会让克隆起步失效，所以
	// fork 开关看 fuseBlocked，横幅看 degraded。
	fuseBlocked bool
	// fuseErrKey 是自检失败的 core 错误键：横幅文案要按"组件缺失 / 写层
	// 不可写 / 挂载被拒"分流，不能把没装组件说成权限问题。
	fuseErrKey    string
	softDismissed bool
	// softReason 记着当前横幅是哪一种原因。原因变化时重置"了解"——用户收掉
	// 的是上一条消息，不能让它在环境恶化后继续保持沉默。
	softReason string
	envChecked bool

	apps   []core.App
	groups []*adw.PreferencesGroup
	// onGroups/offGroups 记录这一轮渲染的组分别挂在哪个视图页，重建时按页
	// Remove（PreferencesPage.Remove 只认自己页上的组）；groups 仍按
	// 分身→所有应用的顺序保存全部组，供回归断言索引。
	onGroups  []*adw.PreferencesGroup
	offGroups []*adw.PreferencesGroup
	query     string
	expanded  bool
	reloadNo  int  // 全量扫描代数：只由 reload 推进
	refreshNo int  // 运行态轮询代数：只由 refreshRunning 推进
	loaded    bool // 首轮扫描是否已经落到界面上
	// suppress 挡住"程序把开关拨回去"触发的 notify，避免取消操作后
	// 又走进一次开启流程（那是死循环，不是交互）。
	suppress map[string]bool
	// pending 挡住一次拨动引发的重复 notify：GtkSwitch 的 active 变化可能
	// 连发多条信号，没有它会在关闭开关时叠出两个确认框。
	pending map[string]bool
	// toggled 记下"这一轮渲染里已经受理过的目标值"。AdwSwitchRow 的 active
	// 绑定会在开关动画结束时对同一个值再发一次 notify::active；只靠 pending
	// 是时序守卫——流程若在补发之前跑完（确认+创建+pop 只要几百毫秒，自动化
	// 与主循环饥饿都可能），pending 已清，同一个开关会二进流程、再开一个
	// 配置页。这里按（渲染代数, 应用, 目标值）去重：行重建后是合法的新操作，
	// 回弹（revert）会删掉表项，失败后用户可以重新拨。
	toggled map[toggleMark]bool
	// 当前压在最上面的探测页/详情页。页面被返回按钮 pop 时靠 pages 收尾，
	// 这两个指针只用于"当前该刷新谁"与去重。
	activeSetup  *setupView
	activeDetail *detailView
	// profileWarns 保存本次运行里 DeriveProfile 产生的档案告警（不落盘，
	// 只用于确认页横幅与刚创建后的详情页解释"范围为什么变了"）。
	profileWarns map[string][]string

	// cb 是全部信号回调的唯一入口（见 handlers.go 的回调纪律）。
	cb *callbacks
	// pages 把导航页指针映射到 pop 后的收尾函数：探测页要停会话并回弹开关，
	// 详情页要清指针并注销处理器。用页面指针而不是"猜当前是哪一页"，
	// 是因为 pop 可能来自屏幕手势，而收尾必须对应到具体那一页。
	pages map[uintptr]func()
	// pageSeq 给每次 push 的页一个唯一 tag。AdwNavigationView 在 pop 过渡
	// 期间仍持有旧页的 tag；紧接着 push 同名 tag 的新页会被拒绝（只打
	// CRITICAL，页面不上屏而状态机照跑——用户表现为开关/详情点不开）。
	// tag 没有别的用途（全仓没有按 tag 查找），唯一即可。
	pageSeq int
	// listScope 是列表页这一轮渲染登记的处理器；重建前整体注销。
	listScope *renderScope
	// listGen 是列表渲染代数。异步流程（探测失败回弹开关、关闭开关确认框）
	// 会跨多秒，期间列表可能已重建、旧行连同整棵子树被 Unref 释放——
	// 拿着旧行指针再 SetActive 就是 use-after-free（P1，复审发现）。
	// 行只在代数未变时才可以直接改；变了就以数据为准（界面已经重建过）。
	listGen int

	// busy 是正在跑的删除事务数。删除要列实例、逐个 gio trash、再刷新列表，
	// 与 setup 的 creating 同属事务态：进程被关窗带走会留下"删了一半"的
	// 实例目录。Hold 让 GApplication 活到事务结束，关窗则直接拒绝。
	busy int

	// 搜索防抖用的一次性定时源（字段持有 → C 槽只有一个）。
	searchFn    glib.SourceFunc
	searchTimer uint32

	// keep 只保活"取过地址、交给 puregotk"的回调变量：它的信号 trampoline 在
	// 触发时会解引用那个地址，变量被回收就是读已释放内存。
	// 控件包装**不用**保活：puregotk 不给包装装 finalizer，C 侧自己持有对象，
	// 而处理器表里的闭包在登记期间持有相关包装（保活控件反而会让 keep 随每次
	// 渲染无限增长——搜索每敲一个字就重建一次列表）。
	keepMu sync.Mutex
	keep   []any
}

func (u *ui) hold(v ...any) {
	u.keepMu.Lock()
	u.keep = append(u.keep, v...)
	u.keepMu.Unlock()
}

// goIdle 把一次性的后台结果切回主线程。可以放心地从后台线程调用：
// 任务进表（带锁），C 槽由 cb.idleFn 独占，不会随调用次数增长。
func (u *ui) goIdle(fn func(uintptr) bool) { u.cb.idleAdd(fn) }

func (u *ui) tr(key string, args map[string]any) string {
	return i18n.T(u.lang, key, args)
}

func (u *ui) toastMsg(s string) {
	t := adw.NewToast(s)
	t.SetTimeout(3)
	u.toast.AddToast(t)
}

// pushPage 压入导航页并登记收尾函数。收尾在 popped 信号里执行（页面真的
// 离开栈之后），所以在收尾里 push 新页面不会和 pop 动画打架。
func (u *ui) pushPage(page *adw.NavigationPage, cleanup func()) {
	if cleanup != nil {
		u.pages[page.GoPointer()] = cleanup
	}
	// 唯一 tag：pop 过渡期间旧页还没离开栈，同名 tag 会让 Push 被
	// AdwNavigationView 静默拒绝（P1，对抗性复审）。
	u.pageSeq++
	page.SetTag("page-" + strconv.Itoa(u.pageSeq))
	u.nav.Push(page)
}

// releasePage 释放我们持有的导航页 sink 引用。push 的 page 是 transfer none
// （nav 自己引用它），而 puregotk 构造器已经 ref_sink 过一次；popped 之后
// 不 Unref，每进一次详情/探测页就泄漏整页（含子树）。
//
// 延后一个 idle：popped 信号发出时页面可能还在信号分发的引用链上，
// 在信号处理函数里直接 Unref 有把对象释放给正在使用的代码的风险。
func (u *ui) releasePage(page *adw.NavigationPage) {
	if page == nil {
		return
	}
	u.goIdle(func(uintptr) bool {
		page.Unref()
		return false
	})
}

// beginBusy/endBusy 包住主线程发起、后台执行的事务（目前是删除）。两个函数
// 都只在主线程调用：Hold/Release 改的是 GApplication 的 use count，与创建
// 窗口、销毁窗口同一条线程才安全。
func (u *ui) beginBusy() {
	u.busy++
	u.app.Hold()
}

func (u *ui) endBusy() {
	u.busy--
	u.app.Release()
}

// toggleMark 是 toggled 的键：同一轮渲染里的同一个开关才算"同一次拨动"。
type toggleMark struct {
	gen int
	key string
}

// newUI 是 GUI 的唯一构造入口：语言在这里按 ADR-025 的规则定一次。
// 集中在一个函数里，回归才能验证"界面语言确实跟随 LANG"，而不是构造
// 语句散落在 main 里、只能靠读代码相信。
func newUI(app *adw.Application, env *core.Env) *ui {
	return &ui{app: app, env: env, lang: i18n.Lang()}
}

// buildWindow 只做一次：根页是列表，详情/探测页按需 push。
func (u *ui) buildWindow() {
	u.suppress = map[string]bool{}
	u.pending = map[string]bool{}
	u.toggled = map[toggleMark]bool{}
	u.profileWarns = map[string][]string{}
	u.cb = newCallbacks()
	u.pages = map[uintptr]func(){}

	u.win = adw.NewApplicationWindow(&u.app.Application)
	u.win.SetTitle(u.tr("ui.window.title", nil))
	// 与设计稿的 880px 对齐：560 宽在桌面窗口管理器里就是一根竖条，列表行
	// 的副标题、切换器与行尾动作会挤成一团。
	u.win.SetDefaultSize(880, 720)

	u.nav = adw.NewNavigationView()

	// 两个平级视图：分身 / 所有应用。各自一个 PreferencesPage（自己滚动），
	// 组之间不再互相挤占滚动位置。
	u.stack = adw.NewViewStack()
	u.onBox = gtk.NewBox(gtk.OrientationVerticalValue, 0)
	u.offBox = gtk.NewBox(gtk.OrientationVerticalValue, 0)
	u.onPage = adw.NewPreferencesPage()
	u.onPage.SetVexpand(true)
	u.offPage = adw.NewPreferencesPage()
	u.offPage.SetVexpand(true)

	u.header = adw.NewHeaderBar()
	u.search = gtk.NewSearchEntry()
	u.search.SetPlaceholderText(u.tr("ui.search.placeholder", nil))
	u.searchFn = func(uintptr) bool {
		u.searchTimer = 0
		q := strings.ToLower(strings.TrimSpace(u.search.GetText()))
		if q == u.query {
			return false
		}
		u.query = q
		u.renderList()
		return false
	}
	// 窗口级回调只建一次（地址恒定），符合 handlers.go 的回调纪律。
	searchcb := func(_ gtk.SearchEntry) {
		if u.searchTimer != 0 {
			glib.SourceRemove(u.searchTimer)
		}
		u.searchTimer = glib.TimeoutAdd(searchDebounceMs, &u.searchFn, 0)
	}
	u.search.ConnectSearchChanged(&searchcb)
	// 标题栏只放标题与应用菜单，搜索条单独一行（见下面的 searchBox）。
	u.header.SetShowTitle(true)

	tv := adw.NewToolbarView()
	tv.AddTopBar(&u.header.Widget)

	// 列表页两条横幅（§4.5）：bwrap 缺失是致命态（整个列表只读），缺 fuse 是
	// 降级态（探测仍可用，只是覆盖不完整）。两者互斥——bwrap 都没有时谈
	// "fuse 降级"没有意义。致命横幅没有关闭按钮：环境没好，"知道了"不能
	// 把状态解除。
	u.fatalBanner = adw.NewBanner(u.tr("ui.banner.fatal", nil))
	u.fatalBanner.SetRevealed(false)
	u.softBanner = adw.NewBanner(u.tr("ui.banner.degraded", nil))
	u.softBanner.SetButtonLabel(u.tr("ui.banner.dismiss", nil))
	u.softBanner.SetRevealed(false)
	u.softDismissFn = func(adw.Banner) {
		u.softDismissed = true
		u.softBanner.SetRevealed(false)
	}
	u.softBanner.ConnectButtonClicked(&u.softDismissFn)

	// 「所有应用」视图：搜索固定在顶部，列表在下面独立滚动。
	searchBox := gtk.NewBox(gtk.OrientationHorizontalValue, 0)
	searchBox.SetMarginStart(12)
	searchBox.SetMarginEnd(12)
	searchBox.SetMarginTop(6)
	searchBox.SetMarginBottom(6)
	u.search.SetHexpand(true)
	searchBox.Append(&u.search.Widget)
	u.offBox.Append(&searchBox.Widget)
	u.offBox.Append(&u.offPage.Widget)

	// 「分身」视图：空态与已开启列表。空态只取代这一视图的列表，不取代
	// 「所有应用」——后者是首启用户唯一能操作的入口。
	u.emptyPage = adw.NewStatusPage()
	u.emptyPage.SetVexpand(true)
	u.emptyPage.SetHexpand(true)
	u.emptyPage.SetIconName("edit-copy-symbolic")
	u.emptyPage.SetTitle(u.tr("ui.empty.title", nil))
	u.emptyPage.SetDescription(u.tr("ui.empty.description", nil))
	pick := gtk.NewButtonWithLabel(u.tr("ui.empty.pick", nil))
	pick.AddCssClass("suggested-action")
	pick.SetHalign(gtk.AlignCenterValue)
	pickcb := func(gtk.Button) { u.stack.SetVisibleChildName("all") }
	pick.ConnectClicked(&pickcb)
	u.hold(&pickcb)
	u.emptyPage.SetChild(&pick.Widget)
	u.onBox.Append(&u.emptyPage.Widget)
	// 首轮 renderList 之前先别让空的 PreferencesPage 占位。
	u.onPage.SetVisible(false)
	u.onBox.Append(&u.onPage.Widget)

	// 图标让两个视图在标题栏里一眼可分；badge 在「分身」页上显示数量，
	// 用户停在「所有应用」时也知道已经有几个分身。
	if p := u.stack.AddTitledWithIcon(&u.onBox.Widget, "on", u.tr("ui.view.clones", nil), "edit-copy-symbolic"); p != nil {
		u.onPageObj = p
		p.Unref()
	}
	if p := u.stack.AddTitledWithIcon(&u.offBox.Widget, "all", u.tr("ui.view.all", nil), "view-app-grid-symbolic"); p != nil {
		p.Unref()
	}
	// 默认落在「所有应用」：首次启动唯一能做的动作是点亮开关；空态里还有
	// 「选择应用」按钮，两个视图互通。
	u.stack.SetVisibleChildName("all")
	// 首轮扫描落地前先别画：已有分身的用户会先看到一闪而过的「所有应用」，
	// 再被切到「分身」——落点按状态决定，就该一次画对。扫描回来后恢复显示。
	u.stack.SetVisible(false)

	u.switcher = adw.NewViewSwitcher()
	u.switcher.SetStack(u.stack)
	u.switcher.SetPolicy(adw.ViewSwitcherPolicyWideValue)
	// 首轮扫描落地前禁用：内容区此时是空的（视图栈还没显示），切视图没有
	// 意义；而且落点还没决定，允许用户先选再被切走是糟糕的体验。
	u.switcher.SetSensitive(false)
	u.header.SetTitleWidget(&u.switcher.Widget)

	rootBox := gtk.NewBox(gtk.OrientationVerticalValue, 0)
	rootBox.Append(&u.fatalBanner.Widget)
	rootBox.Append(&u.softBanner.Widget)
	u.stack.SetVexpand(true)
	rootBox.Append(&u.stack.Widget)
	tv.SetContent(&rootBox.Widget)
	rootPage := adw.NewNavigationPage(&tv.Widget, u.tr("ui.window.title", nil))
	rootPage.SetTag("root")
	u.nav.Push(rootPage)

	// 页面被 pop（HeaderBar 返回按钮 / 屏幕手势）时收尾：探测页要停会话并
	// 回弹开关，详情页要清指针并注销处理器。按页面指针查表，不猜当前状态。
	poppedcb := func(_ adw.NavigationView, page uintptr) {
		cleanup := u.pages[page]
		delete(u.pages, page)
		if cleanup != nil {
			cleanup()
		}
	}
	u.nav.ConnectPopped(&poppedcb)
	u.hold(&poppedcb)

	u.toast = adw.NewToastOverlay()
	u.toast.SetChild(&u.nav.Widget)
	u.win.SetContent(&u.toast.Widget)

	// Esc 语义（§4.6）：确认框由对话框自己处理；探测页的 Esc 等于「取消」，
	// 其余页面**不返回**——返回靠 HeaderBar 后退按钮。AdwNavigationView
	// 默认会把 Esc 当 pop，必须在窗口级截住，否则用户按 Esc 会误以为操作被取消。
	// 先关掉 nav 自带的 pop-on-escape（1.4+）：只靠窗口控制器的传播顺序挡住它
	// 是隐式的——快捷键控制器挂在 nav 上，焦点在子控件时会在冒泡阶段先于
	// 窗口控制器触发。关闭后**页面导航**不再有第二个 Esc 处理者（对话框与
	// 搜索框仍按各自控件语义消费 Esc），语义不再取决于控件树深度。
	// 边界：Alt+←、鼠标返回键与触摸手势仍由 AdwNavigationView 原生处理。
	u.nav.SetPopOnEscape(false)
	esc := gtk.NewEventControllerKey()
	esccb := func(_ gtk.EventControllerKey, keyval uint32, _ uint32, _ gdk.ModifierType) bool {
		if int32(keyval) != gdk.KEY_Escape {
			return false
		}
		if v := u.activeSetup; v != nil {
			// 创建中是事务态：§6.1 要求 Esc 也不生效（半成品比多点一次更糟）。
			if v.state != "creating" {
				v.finish(false)
			}
		}
		return true
	}
	esc.ConnectKeyPressed(&esccb)
	u.win.AddController(&esc.EventController)

	// 只保活取过地址的回调变量：puregotk 的信号 trampoline 在触发时
	// 会解引用它们，变量被 GC 掉就是读已释放内存。控件本身由 C 侧持有，
	// 不需要 Go 侧锚定（锚定反而会随每次渲染无限增长）。
	u.hold(&searchcb, &esccb)

	// 关窗时先取消正在跑的探测：Probe 的 defer 会卸载 overlay、按规则
	// 保留/清掉写层；不取消就退出，写层根下会留下挂载点与现场。
	// 创建/保存是事务态：关窗不能把事务打断在半路（半成品比多等一秒更糟），
	// 与 Esc、返回手势同一条规则（§6.1）。
	closecb := func(_ gtk.Window) bool {
		// 删除事务同样不能在半路被关窗带走（数据目录删一半比多等一秒糟）。
		if u.busy > 0 {
			u.toastMsg(u.tr("ui.toast.deleting", nil))
			return true
		}
		if v := u.activeSetup; v != nil {
			if v.state == "creating" {
				u.toastMsg(u.tr("ui.toast.finishing", nil))
				return true
			}
			v.done = true
			if v.cancel != nil {
				v.cancel()
			}
			u.activeSetup = nil
		}
		return false
	}
	u.win.ConnectCloseRequest(&closecb)
	u.hold(&closecb)

	u.win.Present()
	u.reload()
	u.checkEnv()
	// 运行状态轮询（§4.7）：1 秒一拍，详情页每拍刷、列表每两拍刷——
	// 用户在应用里退出分身，详情页的「删除/启动」要在一秒内解禁；列表的
	// 「M 个运行中」晚一秒无所谓，没必要每秒扫一遍实例目录。
	// 窗口不可见（最小化/在别的桌面）时整拍跳过：没人看的界面不值得开文件。
	var tick int
	pollcb := glib.SourceFunc(func(uintptr) bool {
		if !u.win.GetVisible() {
			return true
		}
		tick++
		if v := u.activeDetail; v != nil {
			v.refresh()
		}
		if tick%2 == 0 {
			u.refreshRunning()
		}
		// 每 30 秒重跑一次环境自检：bwrap 装好之后致命态必须能自己解除
		// （只能进不能出的状态等于卡死，§4.5）。结果没变时不会重建列表。
		if tick%30 == 0 {
			u.checkEnv()
		}
		return true
	})
	u.hold(&pollcb)
	glib.TimeoutAdd(1000, &pollcb, 0)
}

// checkEnv 是启动自检（§4.5）：后台跑一次，结果回主线程决定横幅与列表是否
// 可操作。bwrap 用 LookPath 判定；fuse 用与探测**同一份** mounter 实现判定——
// 自检和探测各写一套"能不能用"，迟早出现"横幅说能用、探测却降级"。
//
// 它也能从致命态回到可用态（tech-stack §4.5 第 245 行的要求：只能进不能出
// 的状态等于卡死）：轮询会周期性重跑，环境恢复后横幅收起、开关重新可用。
func (u *ui) checkEnv() {
	go func() {
		bwrapErr := core.CheckBwrap()
		var fuseErr error
		if bwrapErr == nil {
			fuseErr = u.env.OverlayAvailable()
		}
		u.goIdle(func(uintptr) bool {
			u.envChecked = true
			wasFatal := u.fatal
			u.fatal = bwrapErr != nil
			u.fuseBlocked = bwrapErr == nil && fuseErr != nil
			u.fuseErrKey = core.ProbeErrorKey(fuseErr)
			// 降级横幅只在"真的会改用 strace"时出现；挂载被拒是错误，
			// 由探测页直接展示可操作提示，不能谎称已降级（ADR-032）。
			u.degraded = bwrapErr == nil && core.ProbeFallsBackToStrace(fuseErr)
			u.applyEnvBanners()
			// 确认页可能先于自检落地（open/clone 直接进确认态）：开关的
			// 可用性不能只在渲染那一刻取快照。环境变化时同步一次；降级
			// 后已勾选的克隆起步一并撤销——它此刻不可能生效，留着只会
			// 让用户在不知情的情况下得到一个空实例。
			if v := u.activeSetup; v != nil && v.forkRow != nil {
				if u.fuseBlocked && v.fork {
					v.fork = false
					v.forkRow.SetActive(false)
				}
				v.forkRow.SetSensitive(!u.fuseBlocked)
			}
			if wasFatal != u.fatal {
				// 只有可操作性变了才重建列表：每 30 秒无条件重建会打断滚动与焦点。
				u.renderList()
			}
			return false
		})
	}()
}

func (u *ui) applyEnvBanners() {
	u.fatalBanner.SetRevealed(u.fatal)
	reason := u.softReasonNow()
	if reason != u.softReason {
		u.softReason = reason
		u.softDismissed = false
	}
	if reason != "" {
		u.softBanner.SetTitle(u.bannerTitle(reason))
	}
	u.softBanner.SetRevealed(reason != "" && !u.fatal && !u.softDismissed)
	// 致命态下搜索也停用：搜出一排点不动的开关，只会让用户以为程序坏了。
	u.search.SetSensitive(!u.fatal)
}

// allocText 把绝对路径换成 ~ 形式，只用于展示。
// bannerTitle 用显式 switch 而不是把键前缀与原因拼接：i18n 测试会扫
// tr() 的字面量键，动态拼接既查不到缺翻译，也让键名无法静态核对。
func (u *ui) bannerTitle(reason string) string {
	switch reason {
	case "degraded":
		return u.tr("ui.banner.degraded", nil)
	case "workroot_unavailable":
		return u.tr("ui.banner.workroot_unavailable", nil)
	case "backend_missing":
		return u.tr("ui.banner.backend_missing", nil)
	default:
		return u.tr("ui.banner.fuse_blocked", nil)
	}
}

// softReasonNow 把自检结果归成横幅原因（也是 i18n 键后缀）。三类的处置
// 完全不同：装组件 / 修写层目录 / 修挂载权限，不能合并成一句"兼容模式"。
func (u *ui) softReasonNow() string {
	switch {
	case u.degraded:
		return "degraded"
	case u.fuseErrKey == "err.probe.workroot_unavailable" || u.fuseErrKey == "err.env.mkdir":
		return "workroot_unavailable"
	case u.fuseBlocked && core.ProbeBackendKeyMissing(u.fuseErrKey):
		return "backend_missing"
	case u.fuseBlocked:
		return "fuse_blocked"
	}
	return ""
}

func (u *ui) allocText(p string) string {
	home := u.env.Resolver.Home
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// appIcon 把 .desktop 的 Icon 值变成行前缀。Icon 规范允许主题名或绝对
// 路径，两种都要认。
//
// 取不到时不再用 application-x-executable：一堆应用共用同一个灰色可执行
// 文件图标，30 行看起来像同一行，用户也分不清谁是谁。改用 AdwAvatar 的
// 首字母色块——libadwaita 原生组件，颜色由文本稳定派生，既区分应用，也
// 不会把缺失画成破图占位。
// 第二个返回值表示是否退回首字母头像（fallback）：调用方不需要，但它是
// GTK 闸门唯一能在不依赖像素/类型自省的情况下钉住回退分支的出口。
func (u *ui) appIcon(icon, name string, size int32) (*gtk.Widget, bool) {
	if img, ok := u.iconImage(icon); ok {
		img.SetPixelSize(size)
		img.SetValign(gtk.AlignCenterValue)
		return &img.Widget, false
	}
	av := adw.NewAvatar(size, avatarInitial(name), true)
	av.SetValign(gtk.AlignCenterValue)
	return &av.Widget, true
}

// iconImage 只在图标真的能取到时返回 Image：主题名先过 IconTheme.HasIcon，
// 否则 GTK 会画一个破图，比没有图标更难看。
func (u *ui) iconImage(icon string) (*gtk.Image, bool) {
	switch {
	case icon == "":
		return nil, false
	case filepath.IsAbs(icon):
		if _, err := os.Stat(icon); err != nil {
			return nil, false
		}
		return gtk.NewImageFromFile(icon), true
	default:
		// DisplayGetDefault / IconThemeGetForDisplay 都给返回的包装 IncreaseRef，
		// 按本项目所有权纪律用完必须 Unref（它们是 per-display 单例，不 Unref
		// 不会泄漏对象本身，但引用计数会一直涨）。
		display := gdk.DisplayGetDefault()
		if display == nil {
			return nil, false
		}
		defer display.Unref()
		theme := gtk.IconThemeGetForDisplay(display)
		if theme == nil {
			return nil, false
		}
		defer theme.Unref()
		if !theme.HasIcon(icon) {
			return nil, false
		}
		return gtk.NewImageFromIconName(icon), true
	}
}

// avatarInitial 给出头像里的一个字：中文取首字，拉丁字母转大写。
// 名字为空（理论上不会发生）时用「?」，不让 Avatar 退化成空白圆。
func avatarInitial(name string) string {
	for _, r := range strings.TrimSpace(name) {
		return string(unicode.ToUpper(r))
	}
	return "?"
}

// iconOf 按应用 key 找 Icon，供探测页显示"正在跟踪谁"。
func (u *ui) iconOf(key string) string {
	for _, a := range u.apps {
		if a.Key == key {
			return a.Icon
		}
	}
	return ""
}

// symbolicIcon 是行内 16px 符号图标（实例行、路径行）。libadwaita 的行
// prefix 用这个尺寸，和文字基线对齐。
func (u *ui) symbolicIcon(name string) *gtk.Image {
	img := gtk.NewImageFromIconName(name)
	img.SetPixelSize(16)
	img.SetValign(gtk.AlignCenterValue)
	return img
}

// iconButton 建一个图标+文字的按钮（AdwButtonContent）。两个返回对象都要由
// 调用方登记释放：ButtonContent 挂进按钮后，构造器那份额外引用仍归我们。
func (u *ui) iconButton(icon, label string) (*gtk.Button, *adw.ButtonContent) {
	btn := gtk.NewButton()
	bc := adw.NewButtonContent()
	bc.SetIconName(icon)
	bc.SetLabel(label)
	btn.SetChild(&bc.Widget)
	return btn, bc
}

func (u *ui) appSub(a core.App, on bool) string {
	if !on {
		switch {
		case !a.HasProfile:
			return u.tr("ui.app.subtitle.no_profile", nil)
		case !a.Creatable:
			return u.tr("ui.app.subtitle.stale", nil)
		default:
			return u.tr("ui.app.subtitle.has_profile", nil)
		}
	}
	if a.RunningN > 0 {
		return u.tr("ui.app.subtitle.running", map[string]any{"n": a.InstanceN, "running": a.RunningN})
	}
	return u.tr("ui.app.subtitle.count", map[string]any{"n": a.InstanceN})
}

// reload 重新扫描应用与实例。扫描放到后台，结果回主线程再碰控件。
func (u *ui) reload() {
	u.reloadNo++
	gen := u.reloadNo
	go func() {
		apps, _ := u.env.ListApps(context.Background())
		u.goIdle(func(uintptr) bool {
			if gen != u.reloadNo {
				return false // 已有更新的一轮，丢弃旧结果
			}
			first := !u.loaded
			u.apps = apps
			if first {
				// 首屏落地视图按状态选：已经有分身的人多半是来启动/管理它们的；
				// 一个都没有时留在「所有应用」，因为点亮开关是唯一能做的事。
				// 只决定首屏一次——之后的 reload 是操作结果，不该把用户弹走。
				for _, a := range apps {
					if a.InstanceN > 0 {
						u.stack.SetVisibleChildName("on")
						break
					}
				}
			}
			u.loaded = true
			u.renderList()
			if first {
				// 一开窗就让搜索框抢到焦点会让人以为必须先输入；首轮落地后把
				// 焦点交给当前视图的滚动页（不是 ViewStack——它没有滚动控制器，
				// 方向键/PageDown 滚不动列表）。
				u.focusCurrentView()
			}
			return false
		})
	}()
}

// refreshRunning 只刷新"已有实例"的应用，避免每 2 秒全量扫描 400 个 .desktop。
//
// 它**不能**推进 reloadNo：首屏全量扫描要一两秒，轮询恰好落在这段时间里时
// 会把 reload 的结果判成过期丢掉，窗口就一直空着（真机上复现过）。轮询只
// 跟随当前代数：更新的一轮 reload 会让在途的轮询结果作废，反之不行。
// 另外刷新之间也要用自己的序号，避免两轮轮询乱序覆盖（旧快照后到）。
func (u *ui) refreshRunning() {
	if !u.loaded {
		return // 首轮扫描还没回来，没有可刷新的基准
	}
	gen := u.reloadNo
	u.refreshNo++
	seq := u.refreshNo
	// 在主线程序列化一份快照再交给 goroutine：直接读 u.apps 会和
	// reload 的结果写回并发，是真实的数据竞争（go test -race 会抓）。
	snapshot := make([]core.App, len(u.apps))
	copy(snapshot, u.apps)
	go func() {
		updated := make([]core.App, len(snapshot))
		copy(updated, snapshot)
		for i := range updated {
			if updated[i].InstanceN == 0 {
				continue
			}
			insts, err := u.env.ListInstances(updated[i].Key)
			if err != nil {
				continue
			}
			updated[i].InstanceN = len(insts)
			updated[i].RunningN = 0
			for _, in := range insts {
				if running, _ := u.env.IsRunning(updated[i].Key, in.ID); running {
					updated[i].RunningN++
				}
			}
		}
		u.goIdle(func(uintptr) bool {
			if gen != u.reloadNo || seq != u.refreshNo {
				return false
			}
			if !sameList(u.apps, updated) {
				u.apps = updated
				u.renderList()
			}
			return false
		})
	}()
}

func sameList(a, b []core.App) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].InstanceN != b[i].InstanceN || a[i].RunningN != b[i].RunningN ||
			a[i].HasProfile != b[i].HasProfile || a[i].Creatable != b[i].Creatable {
			return false
		}
	}
	return true
}

// renderList 重建两个视图的组。分组键是"有没有实例"，不是"有没有档案"：
// 关掉开关后应用必须回到「所有应用」，否则组名与行内状态自相矛盾。
func (u *ui) renderList() {
	// 先摘掉旧行，再关旧 scope：scope.close 会 Unref 本轮创建的 C 对象，
	// 而父容器此时的引用还兜着它们——先 Remove 再 Unref 的顺序最不容易出错。
	for _, g := range u.onGroups {
		u.onPage.Remove(g)
	}
	for _, g := range u.offGroups {
		u.offPage.Remove(g)
	}
	u.onGroups, u.offGroups = nil, nil
	u.groups = nil
	if u.listScope != nil {
		u.listScope.close()
	}
	// 旧行从这一刻起不再有效：任何还握着它们指针的异步回调都要放弃写界面。
	u.listGen++
	// 旧代数的受理记录随旧行一起作废（旧行已被 Unref，不会再发 notify），
	// 不清会让表随每轮轮询无限增长。
	for k := range u.toggled {
		if k.gen != u.listGen {
			delete(u.toggled, k)
		}
	}
	sc := &renderScope{c: u.cb}
	u.listScope = sc

	var on, off []core.App
	for _, a := range u.apps {
		if a.InstanceN > 0 {
			on = append(on, a)
			continue
		}
		if u.query == "" || strings.Contains(strings.ToLower(a.Name), u.query) ||
			strings.Contains(strings.ToLower(a.Key), u.query) {
			off = append(off, a)
		}
	}

	// 空态只取代「分身」视图的列表，不影响「所有应用」——后者是首启用户
	// 唯一能操作的入口，空态里的「选择应用」按钮也指向它。
	empty := len(on) == 0
	u.emptyPage.SetVisible(empty)
	u.onPage.SetVisible(!empty)
	if u.onPageObj != nil {
		u.onPageObj.SetBadgeNumber(uint32(len(on)))
	}
	if !empty {
		g := adw.NewPreferencesGroup()
		sc.own(&g.Widget)
		g.SetTitle(u.tr("ui.group.clones.title", nil))
		g.SetDescription(u.tr("ui.group.clones.desc", nil))
		for _, a := range on {
			g.Add(&u.newAppRow(a, true, sc).Widget)
		}
		u.onPage.Add(g)
		u.onGroups = append(u.onGroups, g)
		u.groups = append(u.groups, g)

		// 分身视图底部的「添加应用分身」：不切回另一个视图也能继续加，
		// 与空态的「选择应用」是同一个动作的两种入口。
		ag := adw.NewPreferencesGroup()
		sc.own(&ag.Widget)
		add := adw.NewActionRow()
		sc.own(&add.Widget)
		add.SetTitle(u.tr("ui.row.add_clone.title", nil))
		add.SetSubtitle(u.tr("ui.row.add_clone.subtitle", nil))
		ic := u.symbolicIcon("list-add-symbolic")
		sc.own(&ic.Widget)
		add.AddPrefix(&ic.Widget)
		add.SetActivatable(true)
		sc.row(add, func() { u.stack.SetVisibleChildName("all") })
		ag.Add(&add.Widget)
		u.onPage.Add(ag)
		u.onGroups = append(u.onGroups, ag)
		u.groups = append(u.groups, ag)
	}

	g := adw.NewPreferencesGroup()
	sc.own(&g.Widget)
	// 组名不再重复标题栏上的「所有应用」：视图名已经说了这是全部应用，
	// 再写一遍只是噪声。留一句规则说明就够。
	g.SetDescription(u.tr("ui.group.all.desc", nil))
	pool := off
	var extra *adw.ActionRow
	if u.query == "" && !u.expanded && len(off) > listLimit {
		pool = off[:listLimit]
		extra = u.moreRow(u.tr("ui.row.show_all", map[string]any{"n": len(off)}), sc)
	} else if u.query != "" && len(off) > searchLimit {
		pool = off[:searchLimit]
		// 搜索溢出说明是纯文本，不是可点的「显示全部」：搜索分支不读
		// expanded，做成可点行就是一个点不动的死点击（设计稿也是纯文本）。
		row := adw.NewActionRow()
		row.SetTitle(u.tr("ui.row.search_limit", map[string]any{"shown": searchLimit, "total": len(off)}))
		row.SetSensitive(false)
		sc.own(&row.Widget)
		extra = row
	}
	for _, a := range pool {
		g.Add(&u.newAppRow(a, false, sc).Widget)
	}
	if len(pool) == 0 {
		row := adw.NewActionRow()
		sc.own(&row.Widget)
		if u.query != "" {
			row.SetTitle(u.tr("ui.row.no_match", map[string]any{"query": u.search.GetText()}))
		} else {
			row.SetTitle(u.tr("ui.row.all_clones", nil))
		}
		row.SetSensitive(false)
		g.Add(&row.Widget)
	} else if extra != nil {
		g.Add(&extra.Widget)
	}
	u.offPage.Add(g)
	u.offGroups = append(u.offGroups, g)
	u.groups = append(u.groups, g)

	// 首轮结果已落地、落点也已决定：这时才显示视图栈，避免"先看到所有应用
	// 再跳分身"的闪变；切换器同时解禁（扫描期间它不可点，防止落点覆盖
	// 用户刚做的选择）。放在 renderList 里而不是 reload 的回调里，是为了
	// 结果被更新的一轮丢弃时也不会永久空白。
	u.stack.SetVisible(true)
	u.switcher.SetSensitive(true)
}

// focusCurrentView 把初始焦点交给当前视图的滚动页。
// 不放在 ViewStack 上：它不是滚动容器，方向键/PageDown 滚不动列表。
func (u *ui) focusCurrentView() {
	page := u.onPage
	if u.stack.GetVisibleChildName() == "all" {
		page = u.offPage
	}
	page.Widget.SetFocusable(true)
	page.Widget.GrabFocus()
}

func (u *ui) moreRow(title string, sc *renderScope) *adw.ActionRow {
	row := adw.NewActionRow()
	sc.own(&row.Widget)
	row.SetTitle(title)
	if u.fatal {
		// 致命态下整页只读（§4.5）：行还看得见，但不响应任何点击。
		row.SetSensitive(false)
		return row
	}
	row.SetActivatable(true)
	sc.row(row, func() {
		u.expanded = true
		u.renderList()
	})
	return row
}

// appDisplayName 保证列表行永远有字可显示。.desktop 的 Name 在规范里是必填，
// 但现实里存在只有 Name[某地区] 的条目；localizedName 解不出来时显示 desktop
// id 也比一条空白行好诊断（用户能据此告诉我们"这个名字怪的应用在这"）。
func appDisplayName(a core.App) string {
	if n := strings.TrimSpace(a.Name); n != "" {
		return a.Name
	}
	return a.Key
}

func (u *ui) newAppRow(a core.App, on bool, sc *renderScope) *adw.SwitchRow {
	row := adw.NewSwitchRow()
	sc.own(&row.Widget)
	row.SetTitle(appDisplayName(a))
	row.SetSubtitle(u.appSub(a, on))
	row.SetActive(on)

	// 应用图标是列表里唯一能让人"一眼认出这是哪个应用"的东西；全是文字
	// 时 30 行看起来一模一样。Icon 可能是主题名，也可能是绝对路径，两种
	// 都支持；取不到时用首字母色块，不用千篇一律的通用图标。
	icon, _ := u.appIcon(a.Icon, appDisplayName(a), 32)
	sc.own(icon)
	row.AddPrefix(icon)

	if u.fatal {
		// bwrap 缺失时任何操作最终都会失败：开关停用、箭头不出现，
		// 而不是允许用户点进去走一遍必然失败的路（§4.5）。
		row.SetSensitive(false)
		return row
	}

	// 先设属性再挂回调：属性变化本身也会发 notify，挂在后面等于只监听用户操作。
	ref := rowRef{row: row, gen: u.listGen}
	sc.switchRow(row, func() { u.onSwitch(a, ref, on) })

	if on {
		btn := gtk.NewButtonFromIconName("go-next-symbolic")
		sc.own(&btn.Widget)
		btn.AddCssClass("flat")
		btn.SetTooltipText(u.tr("ui.tooltip.open_detail", nil))
		sc.button(btn, func() { u.pushDetail(a.Key) })
		row.AddSuffix(&btn.Widget)
	}
	return row
}

// rowRef 绑定"哪一轮渲染的哪一行"。异步结果回来时先比代数：列表重建过就
// 不再碰旧控件（它已经被 Unref 释放），真实状态由重建后的界面自行表达。
type rowRef struct {
	row *adw.SwitchRow
	gen int
}

// onSwitch 只在用户真的拨动开关时进入流程：程序回拨会先置 suppress。
func (u *ui) onSwitch(a core.App, ref rowRef, was bool) {
	if u.suppress[a.Key] || u.pending[a.Key] {
		return
	}
	// 旧代数的行已经不在列表里（可能仍被信号分发链暂时保活）：它的状态
	// 属于上一轮渲染，不能拿来开流程。revert 用同一条代数守卫。
	if ref.gen != u.listGen {
		return
	}
	if ref.row.GetActive() == was {
		return
	}
	// 同一个目标值在同一轮渲染里只受理一次：AdwSwitchRow 会在动画结束时
	// 补发 notify::active，重复受理会二进流程（见 toggled 注释）。
	target := ref.row.GetActive()
	mark := toggleMark{gen: ref.gen, key: a.Key}
	if seen, ok := u.toggled[mark]; ok && seen == target {
		return
	}
	u.toggled[mark] = target
	if target {
		u.beginEnable(a, ref)
	} else {
		u.confirmDisable(a, ref)
	}
}

// revert 把开关拨回用户拨动前的状态。行若已随上一轮渲染销毁就什么都不做：
// 那次重建读的是磁盘真实状态，界面本来就是对的。
func (u *ui) revert(ref rowRef, key string, to bool) {
	if ref.gen != u.listGen {
		return
	}
	u.suppress[key] = true
	ref.row.SetActive(to)
	u.suppress[key] = false
	// 开关被程序拨回去了：同一目标值的下次拨动是用户的新操作，不是补发。
	delete(u.toggled, toggleMark{gen: ref.gen, key: key})
}

// beginEnable 按档案状态选入口：没档案 → 首次配置；档案失效 → 重新配置；
// 档案可用 → 直接开启（复用档案，跳过探测）。
func (u *ui) beginEnable(a core.App, ref rowRef) {
	u.pending[a.Key] = true
	mode := "open"
	switch {
	case !a.HasProfile:
		mode = "setup"
	case !a.Creatable:
		mode = "reconfig"
	}
	// id 分配在 core 里做（AllocID 保证"第一个空闲"，删掉中间一个也不会重名）。
	if !u.pushSetup(a.Key, mode, func(ok bool) {
		u.pending[a.Key] = false
		if !ok {
			u.revert(ref, a.Key, false)
			return
		}
		u.reload()
		// 创建完落到详情页；返回时停在「分身」视图——刚建出的实例在那里，
		// 也避免用户回到「所有应用」却发现刚拨的开关"消失"了。
		u.stack.SetVisibleChildName("on")
		u.pushDetail(a.Key)
	}) {
		u.pending[a.Key] = false
		u.revert(ref, a.Key, false)
	}
}

func (u *ui) confirmDisable(a core.App, ref rowRef) {
	u.pending[a.Key] = true
	if a.RunningN > 0 {
		d := adw.NewAlertDialog(u.tr("ui.dialog.running.title", nil), u.tr("ui.dialog.running.body", nil))
		d.AddResponse("ok", u.tr("ui.dialog.ok", nil))
		d.SetDefaultResponse("ok")
		u.cb.onDialog(d, func(_ string) {
			u.pending[a.Key] = false
			u.revert(ref, a.Key, true)
		})
		if dialogHook != nil {
			dialogHook(d)
		}
		d.Choose(&u.win.Widget, nil, nil, 0)
		return
	}
	d := adw.NewAlertDialog(u.tr("ui.dialog.close_clones.title", map[string]any{"name": appDisplayName(a)}), u.tr("ui.dialog.close_clones.body", nil))
	d.AddResponse("cancel", u.tr("ui.dialog.cancel", nil))
	d.AddResponse("trash", u.tr("ui.dialog.trash", nil))
	d.SetDefaultResponse("cancel")
	d.SetResponseAppearance("trash", adw.ResponseDestructiveValue)
	u.cb.onDialog(d, func(resp string) {
		u.pending[a.Key] = false
		if resp != "trash" {
			u.revert(ref, a.Key, true)
			return
		}
		// 事务期间 Hold + 挡住关窗：删一半的实例目录比多等一秒糟。
		u.beginBusy()
		go func() {
			out := u.deleteAllInstances(a.Key)
			u.goIdle(func(uintptr) bool {
				u.endBusy()
				switch {
				case out.openErr != nil:
					// 拿不到写锁/列不出实例不是"有分身在跑"：按真实原因提示，
					// 否则用户会去应用里找一个根本不存在的运行实例。
					u.revert(ref, a.Key, true)
					u.toastMsg(u.tr(errorKey(out.openErr), errorArgs(out.openErr)))
				case out.firstErr != nil:
					// 有失败就说明还有实例没删掉，开关必须回到"开"。
					// 个数必须能对上：deleted + failed + running = 总数，
					// 少报一类会让用户以为"都处理完了"（P3，复审发现）。
					u.revert(ref, a.Key, true)
					var parts []string
					if out.deleted > 0 {
						parts = append(parts, u.tr("ui.toast.trashed_n", map[string]any{"n": out.deleted}))
					}
					parts = append(parts, u.tr("ui.toast.delete_failed", map[string]any{"n": out.failed, "detail": u.tr(errorKey(out.firstErr), errorArgs(out.firstErr))}))
					if out.running > 0 {
						parts = append(parts, u.tr("ui.toast.running_left", map[string]any{"n": out.running}))
					}
					u.toastMsg(strings.Join(parts, u.tr("ui.toast.separator", nil)))
				case out.running > 0:
					u.revert(ref, a.Key, true)
					if out.deleted > 0 {
						u.toastMsg(u.tr("ui.toast.trashed_running", map[string]any{"deleted": out.deleted, "running": out.running}))
					} else {
						u.toastMsg(u.tr("ui.toast.running_only", map[string]any{"n": out.running}))
					}
				default:
					u.toastMsg(u.tr("ui.toast.all_trashed", map[string]any{"name": appDisplayName(a)}))
				}
				u.reload()
				return false
			})
		}()
	})
	if dialogHook != nil {
		dialogHook(d)
	}
	d.Choose(&u.win.Widget, nil, nil, 0)
}

// deleteOutcome 把"删除发生了什么事"拆开记：全成功 / 还有在跑的 / 真删失败 /
// 连实例列表都读不到。合成一个 bool 会让界面用一句"有实例正在运行"掩盖
// 锁超时、gio 缺失、权限不足这些完全不同的原因（CLI 的退出码也是分开的）。
type deleteOutcome struct {
	deleted  int
	running  int // 因运行中而没删的实例数
	failed   int // 因删除本身失败而没删的实例数
	firstErr error
	openErr  error // 拿不到写锁或列不出实例：什么都没做
}

// deleteAllInstances 删除一个应用的全部分身。
//
// 运行态必须**整体**判：确认框弹出后用户可能又启动了一个，若逐个"边删边判"，
// 已停止的实例会被删掉、运行中的被挡住，等于部分执行了一个本应整体拒绝的
// 操作（§6.1「有实例正在运行 → 先拒绝」）。所以先全量检查，任一在跑就
// 一个都不删。
func (u *ui) deleteAllInstances(key string) deleteOutcome {
	var out deleteOutcome
	ctx := context.Background()
	st, err := u.env.OpenState(ctx, 5*time.Second)
	if err != nil {
		out.openErr = err
		return out
	}
	defer st.Close()
	insts, err := u.env.ListInstances(key)
	if err != nil {
		out.openErr = err
		return out
	}
	for _, in := range insts {
		if running, _ := u.env.IsRunning(key, in.ID); running {
			out.running++
		}
	}
	if out.running > 0 {
		return out
	}
	for _, in := range insts {
		if derr := u.env.DeleteInstance(ctx, key, in.ID); derr != nil {
			// 删除期间被启动也算"运行中"：DeleteInstance 拿不到 run.lock 时
			// 报的就是它。区分这两种失败，提示才指得对方向。
			if running, _ := u.env.IsRunning(key, in.ID); running {
				out.running++
				continue
			}
			out.failed++
			if out.firstErr == nil {
				out.firstErr = derr
			}
			continue
		}
		out.deleted++
	}
	return out
}

// setProfileWarns 记下本次运行 DeriveProfile 产生的档案告警（run-scoped，
// 不落盘）。确认页横幅与刚创建后的详情页据此解释"范围为什么变了"。
func (u *ui) setProfileWarns(appKey string, warns []string) {
	if u.profileWarns == nil {
		u.profileWarns = map[string][]string{}
	}
	if len(warns) == 0 {
		delete(u.profileWarns, appKey)
		return
	}
	u.profileWarns[appKey] = append([]string{}, warns...)
}

func (u *ui) profileWarnings(appKey string) []string {
	return u.profileWarns[appKey]
}
