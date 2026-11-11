package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

// setupView 是屏 3 的四态机：probing → confirm → creating → done | error。
//
// creating 是独立态，不是"confirm 里加个 flag"：写档案 + 建目录 + 生成
// .desktop 是几百毫秒的事务，期间再点一次确认会建出两个实例；中途放行取消
// 会留下半成品。
type setupView struct {
	u      *ui
	appKey string
	// mode: setup 首次配置 | reconfig 档案失效 | open 开启分身 |
	// clone 新建分身 | rediscover 只重跑探测并保存档案
	mode    string
	onClose func(success bool)

	page   *adw.NavigationPage
	stack  *gtk.Stack
	banner *adw.Banner
	header *adw.HeaderBar

	cancelBtn  *gtk.Button
	confirmBtn *gtk.Button

	spinner    *gtk.Spinner
	countLabel *gtk.Label
	// createSpinner 只在 creating 态转：切出去还转的 spinner 会让人以为
	// 还有操作没结束，也白白占着每帧的重绘。
	createSpinner *gtk.Spinner
	// errPage 是失败态的 AdwStatusPage（§4.5）：标题按原因分类，描述带下一步。
	errPage *adw.StatusPage

	confirmPage *adw.PreferencesPage
	rows        map[int]*adw.SwitchRow
	lockRow     *adw.SwitchRow
	dbusRow     *adw.SwitchRow
	runtimeRow  *adw.SwitchRow
	// forkRow 是「从现有应用数据克隆起步」开关（§8.1）。默认关，与 CLI
	// `add` 不带 --fork 的默认一致；只有真正建实例的入口才出现。
	forkRow *adw.SwitchRow
	fork    bool

	profile core.Profile
	cands   []core.Candidate
	// existing/bundled 是重定路径时**非路径字段**的来源：重配置只重新定案
	// 路径，env / extra_binds / isolate_runtime / audit / 未知字段是用户或
	// 维护者的显式配置，探测产物里没有它们，不能顺手抹掉。
	existing    core.Profile
	hasExisting bool
	bundled     core.Profile
	hasBundled  bool
	// prefilled 记录内置模板预填了几条路径（0 = 没有模板或没有新增）。
	// 只用于在确认页说明来源，不改变"探测定案"的事实。
	prefilled int
	// readsWarns 是本次探测的读证据告警（run-scoped，不落盘）；readTrace 是
	// 四态。确认页/错误页都从这里取，不在界面层重新推导。
	readsWarns []string
	readTrace  string
	// degraded 记录降级横幅是否已起：读证据告警要拼在它后面，不能把它顶掉。
	degraded bool

	state  string
	cancel context.CancelFunc
	done   bool
	// retry 是失败态「重试」要重做的事：探测失败重探测、保存失败重保存、
	// 创建失败重创建。一律回到 start() 会为一次保存失败白跑整场探测。
	retry func()
	// held 记录本页是否 Hold 了 GApplication：探测与创建/保存事务都必须活到
	// defer/回滚链跑完，窗口关闭不能把进程从半路带走。
	held bool
	// success 是 finish 的结果，由 popped 收尾读出来交给 onClose。
	success bool
	// fixed 是 HeaderBar 两个按钮的处理器（随页面存亡，一次登记）；
	// scope 是确认页这一轮的处理器，重建确认页时整体注销。
	fixed *renderScope
	scope *renderScope
}

func (u *ui) pushSetup(appKey, mode string, onClose func(bool)) bool {
	// 同一时间只允许一个探测页：重复点开关不应该叠出两页。
	if u.activeSetup != nil {
		return false
	}
	v := &setupView{u: u, appKey: appKey, mode: mode, onClose: onClose, rows: map[int]*adw.SwitchRow{}}
	u.activeSetup = v
	v.build()
	u.pushPage(v.page, v.popped)
	v.start()
	return true
}

// popped 是页面真正离开导航栈之后的收尾：停会话、注销处理器、把结果交给
// 调用方。放在 popped 里而不是 Pop() 之后立刻调用，是因为在 pop 动画期间
// 再 push 详情页会撞上导航视图的过渡态（§6.1 要求创建完落到详情页）。
func (v *setupView) popped() {
	if !v.done {
		// 用户用返回按钮/返回手势取消：等同于按了「取消」。
		v.done = true
		if v.cancel != nil {
			v.cancel()
		}
		if v.u.activeSetup == v {
			v.u.activeSetup = nil
		}
	}
	if v.scope != nil {
		v.scope.close()
	}
	if v.fixed != nil {
		v.fixed.close()
	}
	if v.onClose != nil {
		v.onClose(v.success)
	}
	v.u.releasePage(v.page)
}

func (v *setupView) title() string {
	name := v.appName()
	switch v.mode {
	case "reconfig":
		return v.u.tr("ui.setup.title.reconfig", map[string]any{"name": name})
	case "open":
		return v.u.tr("ui.setup.title.open", map[string]any{"name": name})
	case "clone":
		return v.u.tr("ui.setup.title.clone", map[string]any{"name": name})
	case "rediscover":
		return v.u.tr("ui.setup.title.rediscover", map[string]any{"name": name})
	}
	return v.u.tr("ui.setup.title.setup", map[string]any{"name": name})
}

func (v *setupView) build() {
	// fixed 随页面存亡：HeaderBar 按钮与静态控件在这里登记一次，popped 时
	// 统一注销处理器 + Unref（构造器给每个对象 IncreaseRef，包装没有 finalizer）。
	v.fixed = &renderScope{c: v.u.cb}

	v.stack = gtk.NewStack()
	v.stack.SetVexpand(true)
	v.fixed.own(&v.stack.Widget)

	// ---- probing ----
	probing := gtk.NewBox(gtk.OrientationVerticalValue, 12)
	probing.SetValign(gtk.AlignCenterValue)
	probing.SetHalign(gtk.AlignCenterValue)
	probing.SetMarginTop(24)
	probing.SetMarginBottom(24)
	probing.SetMarginStart(24)
	probing.SetMarginEnd(24)
	v.fixed.own(&probing.Widget)
	v.spinner = gtk.NewSpinner()
	v.spinner.SetSizeRequest(32, 32)
	v.spinner.Start()
	v.spinner.SetHalign(gtk.AlignCenterValue)
	v.fixed.own(&v.spinner.Widget)
	// 探测要等十秒到一分钟，页面上先放应用图标，用户一眼确认在等谁。
	// 没有图标的应用也显示首字母色块：空白比通用图标更让人怀疑「是不是选错了应用」。
	img, _ := v.u.appIcon(v.u.iconOf(v.appKey), v.appName(), 64)
	v.fixed.own(img)
	probing.Append(img)
	ptitle := gtk.NewLabel(v.u.tr("ui.setup.probing.title", map[string]any{"name": v.appName()}))
	ptitle.SetWrap(true)
	ptitle.SetHalign(gtk.AlignCenterValue)
	v.fixed.own(&ptitle.Widget)
	v.countLabel = gtk.NewLabel(v.u.tr("ui.setup.probing.count", map[string]any{"n": 0}))
	v.countLabel.AddCssClass("dim-label")
	v.fixed.own(&v.countLabel.Widget)
	phint := gtk.NewLabel(v.u.tr("ui.setup.probing.hint", nil))
	phint.SetWrap(true)
	phint.SetJustify(gtk.JustifyCenterValue)
	phint.SetHalign(gtk.AlignCenterValue)
	phint.AddCssClass("dim-label")
	v.fixed.own(&phint.Widget)
	probing.Append(&v.spinner.Widget)
	probing.Append(&ptitle.Widget)
	probing.Append(&v.countLabel.Widget)
	probing.Append(&phint.Widget)
	v.stack.AddNamed(&probing.Widget, "probing")

	// ---- error（AdwStatusPage：§4.5 要求失败是独立组件而不是两块裸标签）----
	v.errPage = adw.NewStatusPage()
	v.errPage.SetIconName("dialog-warning-symbolic")
	v.errPage.SetTitle(v.u.tr("ui.setup.error.title", nil))
	v.errPage.SetDescription("")
	v.fixed.own(&v.errPage.Widget)
	v.stack.AddNamed(&v.errPage.Widget, "error")

	// ---- creating ----
	creating := gtk.NewBox(gtk.OrientationVerticalValue, 12)
	creating.SetValign(gtk.AlignCenterValue)
	creating.SetHalign(gtk.AlignCenterValue)
	v.fixed.own(&creating.Widget)
	cspinner := gtk.NewSpinner()
	cspinner.SetSizeRequest(32, 32)
	cspinner.SetHalign(gtk.AlignCenterValue)
	v.createSpinner = cspinner
	v.fixed.own(&cspinner.Widget)
	clabel := gtk.NewLabel(v.u.tr("ui.setup.creating", nil))
	clabel.SetHalign(gtk.AlignCenterValue)
	v.fixed.own(&clabel.Widget)
	creating.Append(&cspinner.Widget)
	creating.Append(&clabel.Widget)
	v.stack.AddNamed(&creating.Widget, "creating")

	// ---- confirm（内容在 renderConfirm 里重建）----
	v.confirmPage = adw.NewPreferencesPage()
	v.confirmPage.SetTitle(v.u.tr("ui.setup.confirm.title", nil))
	v.fixed.own(&v.confirmPage.Widget)
	v.stack.AddNamed(&v.confirmPage.Widget, "confirm")

	v.banner = adw.NewBanner("")
	v.banner.SetRevealed(false)
	v.fixed.own(&v.banner.Widget)

	v.header = adw.NewHeaderBar()
	v.fixed.own(&v.header.Widget)
	v.cancelBtn = gtk.NewButtonWithLabel(v.u.tr("ui.setup.cancel", nil))
	v.confirmBtn = gtk.NewButtonWithLabel(v.u.tr("ui.setup.confirm_button.setup", nil))
	v.confirmBtn.AddCssClass("suggested-action")
	v.fixed.own(&v.cancelBtn.Widget)
	v.fixed.own(&v.confirmBtn.Widget)

	// HeaderBar 的两个按钮活到页面结束：处理器登记一次，随 popped 注销。
	v.fixed.button(v.cancelBtn, func() { v.finish(false) })
	v.fixed.button(v.confirmBtn, func() { v.submit() })
	v.header.PackStart(&v.cancelBtn.Widget)
	v.header.PackEnd(&v.confirmBtn.Widget)

	content := gtk.NewBox(gtk.OrientationVerticalValue, 0)
	v.fixed.own(&content.Widget)
	content.Append(&v.banner.Widget)
	content.Append(&v.stack.Widget)

	tv := adw.NewToolbarView()
	v.fixed.own(&tv.Widget)
	tv.AddTopBar(&v.header.Widget)
	tv.SetContent(&content.Widget)
	v.page = adw.NewNavigationPage(&tv.Widget, v.title())
	// 导航页本身不进 scope：scope 在 popped 里立刻 close，而那一刻 pop 动画
	// 可能还没走完。它的 sink 引用由 popped 末尾的 releasePage 延后一个 idle
	// 释放，与详情页同一条规则（design/navspike 已验证；不释放则整页泄漏）。
	v.page.SetCanPop(true)
}

func (v *setupView) appName() string {
	for _, a := range v.u.apps {
		if a.Key == v.appKey {
			return appDisplayName(a)
		}
	}
	return v.appKey
}

func (v *setupView) confirmLabel() string {
	switch v.mode {
	case "open":
		return v.u.tr("ui.setup.confirm_button.open", nil)
	case "rediscover":
		return v.u.tr("ui.setup.confirm_button.rediscover", nil)
	}
	return v.u.tr("ui.setup.confirm_button.setup", nil)
}

func (v *setupView) setState(s string) {
	v.state = s
	v.stack.SetVisibleChildName(s)
	// 创建中是事务态：返回手势也要挡住（§6.1「关闭路径统一判 creating 后拒绝」）。
	v.page.SetCanPop(s != "creating")
	v.confirmBtn.SetVisible(s == "confirm" || s == "error" || s == "creating")
	v.confirmBtn.SetSensitive(true)
	v.cancelBtn.SetSensitive(s != "creating")
	// 按钮文案按状态重置：§6.1 只有探测中叫「取消探测」，确认态与失败态都叫「取消」。
	// 不重置的话，探测失败后按钮还挂着「取消探测」，用户不知道它已经不再是取消会话。
	v.cancelBtn.SetLabel(v.u.tr("ui.setup.cancel", nil))
	// 只有探测中才转圈：切到确认/失败态后后台还在转的 spinner 会让人以为
	// 还有操作没结束（它也确实代表不了任何事）。
	if s == "probing" {
		v.spinner.Start()
	} else {
		v.spinner.Stop()
	}
	if s == "creating" {
		v.createSpinner.Start()
	} else {
		v.createSpinner.Stop()
	}
	switch s {
	case "probing":
		v.confirmBtn.SetVisible(false)
		v.cancelBtn.SetLabel(v.u.tr("ui.setup.cancel_probing", nil))
	case "error":
		// 「重试」恒可用：失败态只有重试与取消两条出路，留一个点不动的按钮
		// 用户只能关窗口（§6.1 ③-d）。没有重试路径的失败（档案版本超前 /
		// 损坏，重跑只会得到同一结果）不显示重试，只留「取消」。
		if v.retry == nil {
			v.confirmBtn.SetVisible(false)
			break
		}
		v.confirmBtn.SetLabel(v.u.tr("ui.setup.retry", nil))
	case "creating":
		v.confirmBtn.SetLabel(v.creatingLabel())
		v.confirmBtn.SetSensitive(false)
	default: // confirm
		v.confirmBtn.SetLabel(v.confirmLabel())
		v.updateConfirmGate()
	}
}

// creatingLabel 区分"在写什么"：保存档案与创建实例耗时不同，用户需要知道
// 自己等的是哪一件事。
func (v *setupView) creatingLabel() string {
	if v.mode == "rediscover" {
		return v.u.tr("ui.setup.saving", nil)
	}
	return v.u.tr("ui.setup.creating_button", nil)
}

// updateConfirmGate 只数「应用自身数据」：全不勾、只勾缓存或单例时禁用。
// 否则会生成一个什么都不隔离的空档案（§6.1 ③-b，设计稿 guitest 有专项断言）。
func (v *setupView) updateConfirmGate() {
	if v.state != "confirm" {
		return
	}
	n := 0
	for i, row := range v.rows {
		if !row.GetActive() || i >= len(v.cands) {
			continue
		}
		// 判据只在 core 一处：cache/locked/公共目录（含既有档案里
		// Class=Config、DisplayClass=Public 的行）都不算数。
		if core.CountsTowardGate(v.cands[i], row.GetActive()) {
			n++
		}
	}
	v.confirmBtn.SetSensitive(n > 0)
}

// start 决定这一页是直接确认（open/clone）还是先探测。
//
// setup / reconfig / rediscover **必须探测**（§6.1 五个入口）：档案路径是本机
// 相关的（原生/Flatpak、版本差异），内置模板只是预填来源，不定案。
func (v *setupView) start() {
	if v.done {
		return
	}
	if v.mode == "open" || v.mode == "clone" {
		p, err := v.u.env.LoadProfile(v.appKey)
		switch {
		case err == nil:
			v.profile = p
			// 复用入口也要展示四态与档案告警：用户开一个 partial/unavailable/
			// 旧档案时，和重新探测看到的是同一份读证据结论。
			v.readTrace = p.ReadTrace
			// 档案 → 候选只有 core 一份实现（§8.3）：缓存/公共/keyring 行与
			// 凭据展示必须和 CLI、重探测一致。
			cands, cwarns := v.u.env.Resolver.CandidatesFromProfileWithWarns(p)
			v.cands = cands
			v.profile.Warnings = mergeWarnKeys(v.profile.Warnings, cwarns)
			v.u.setProfileWarns(v.appKey, v.profile.Warnings)
			v.setState("confirm")
			v.renderConfirm()
			v.showReadsBanner()
			return
		case errorKey(err) == "err.profile.not_found":
			// 列表扫描与用户拨开关之间档案被删（另一个 CLI 执行了
			// --purge-profile，或外部删除）：按"没有旧档案"重新走首次配置，
			// 而不是把用户钉在只有「取消」的错误页上（对抗性复审 P2）。
			// CLI 的 add 用同一口径：not_found 继续探测。
			v.mode = "setup"
			v.page.SetTitle(v.title())
		default:
			// 复用档案失败（版本超前 / 损坏）没有可重试的动作：重跑 start()
			// 只会读到同一份档案。给「重试」等于让用户对着一个永远失败的
			// 按钮点（P3，复审发现）。
			v.showProfileRejected(err)
			return
		}
	}
	// 重新配置/重新探测只重定案**路径**：既有档案的 env、extra_binds、
	// isolate_runtime、audit、未知字段是用户的显式配置，探测产物里没有它们。
	// 不先把它们收好，重建出来的档案会把它们整片抹掉（P1，复审发现）。
	if p, err := v.u.env.LoadProfile(v.appKey); err == nil {
		v.existing = p
		v.hasExisting = true
	} else if errorKey(err) != "err.profile.not_found" {
		// 版本超前 / 档案损坏不是"没有档案"：继续探测会把这份档案覆盖掉
		// （§5.5 要求拒绝加载并提示）。
		v.showProfileRejected(err)
		return
	}
	if b, ok, berr := v.u.env.BundledProfile(v.appKey); berr != nil {
		// 内置模板是编译进二进制的常量，读不出来说明构建坏了：重探也好不了，
		// 所以不给「重试」，只把原因说清楚。描述不复用 i18n 的"请重新探测
		// 生成"——这里恰恰是重探解决不了的场景（复审 P3）。
		v.retry = nil
		v.showErrorTitle(v.u.tr("ui.setup.bundled_error.title", nil), berr)
		v.errPage.SetDescription(v.u.tr("ui.setup.bundled_error.desc", nil))
		return
	} else if ok {
		v.bundled = b
		v.hasBundled = true
	}
	v.probe()
}

// probe 跑一次探测。闸门与探测全在后台：LoadApp 会扫 .desktop、StaticHints
// 会起 flatpak 子进程、OpenState 可能等锁 5 秒——任何一项放主线程，窗口
// 都会在这几秒里画不出帧。
func (v *setupView) probe() {
	// pop 动画期间错误页上的「重试」仍可能被点到：页面已经 finish，再跑
	// 一场探测会重新挂 overlay、起 dbus、甚至再拉起一次应用，而结果只会
	// 被 done 检查丢掉。入口处挡掉（对抗复审 P3）。
	if v.done {
		return
	}
	v.setState("probing")
	v.countLabel.SetText(v.u.tr("ui.setup.probing.count", map[string]any{"n": 0}))

	ctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel

	// 探测期间 Hold 住 GApplication：用户直接关窗时 use count 若归零，
	// main 会在 Probe 的 defer 之前退出——overlay 没人卸载、setsid 出来的
	// 应用也不会收到 SIGTERM。Release 在探测返回后的 idle 里做（P1，复审发现）。
	v.hold()

	go func() {
		fail := func(err error) {
			v.u.goIdle(func(uintptr) bool {
				v.release()
				if v.done {
					return false
				}
				if errors.Is(err, core.ErrCanceled) {
					v.notifyCanceledScene(err)
					v.finish(false)
					return false
				}
				v.retry = v.retryAction(err)
				v.showError(err)
				return false
			})
		}

		d, err := v.u.env.LoadApp(v.appKey)
		if err != nil {
			fail(err)
			return
		}
		// 闸门 1：bwrap 缺失时探测根本没有意义，不该先启动一次会话再报
		// "会话没能跑起来"这种看不出原因的错（§6.1 四道闸门）。
		if err := core.CheckBwrap(); err != nil {
			fail(err)
			return
		}
		// 闸门 4：全局写锁。探测会改命名空间（挂 overlay、写 profiles.d），
		// 必须与另一个 appclone 的 add/rm 串行；等锁超时按 err.lock.timeout
		// 报"另一个 appclone 正在操作"，而不是一条模糊的探测失败。
		st, err := v.u.env.OpenState(ctx, 5*time.Second)
		if err != nil {
			fail(err)
			return
		}
		defer st.Close()

		// 闸门 2/3（fuse 降级、应用在跑）由 core.Probe 判定：降级不阻断，
		// app_running 直接失败。自检与探测各写一套判据迟早会说不到一处去。
		hints := v.u.env.StaticHints(ctx, d)
		cmdline := append([]string{d.Exec}, d.ExecArgs...)
		res, perr := v.u.env.Probe(ctx, v.appKey, core.ProbeOptions{
			Command:     cmdline,
			LockFile:    hints.LockFiles,
			StaticPaths: hints.Paths,
			Progress: func(pp core.ProbeProgress) {
				v.u.goIdle(func(uintptr) bool {
					if v.done {
						return false
					}
					// 降级横幅从探测中起常驻，切到确认态也不消失——用户要在
					// 确认路径范围之前就知道这份结果不完整（§4.5）。
					if pp.Stage == "fallback" {
						v.showDegraded()
						return false
					}
					if v.state != "probing" {
						return false
					}
					if pp.Reads > 0 {
						v.countLabel.SetText(v.u.tr("ui.setup.probing.count_reads",
							map[string]any{"n": pp.Entries, "r": pp.Reads}))
					} else {
						v.countLabel.SetText(v.u.tr("ui.setup.probing.count", map[string]any{"n": pp.Entries}))
					}
					return false
				})
			},
			OnReadsWarn: func(reason string) {
				// 读证据不完整必须在探测过程中就告诉用户，而不是等确认页
				// 落定后才补一条横幅（§11.1）。
				v.u.goIdle(func(uintptr) bool {
					if v.done {
						return false
					}
					v.addReadsWarn(reason)
					return false
				})
			},
		})
		v.u.goIdle(func(uintptr) bool {
			v.release()
			if v.done {
				return false
			}
			if perr != nil {
				if errors.Is(perr, core.ErrCanceled) {
					v.notifyCanceledScene(perr)
					v.finish(false)
					return false
				}
				v.retry = v.retryAction(perr)
				v.showError(perr)
				return false
			}
			v.applyProbeResult(res)
			return false
		})
	}()
}

// hold 让进程在事务完成前不被"窗口关闭"结束：GUI 没有 daemon，窗口一销毁
// GApplication 的 use count 归零，进程会在 goroutine 的 defer 之前退出。
// 幂等——探测 → 确认 → 创建是串行事务，同一时刻只有一段在跑。
func (v *setupView) hold() {
	if v.held {
		return
	}
	v.held = true
	v.u.app.Hold()
}

// release 释放探测/创建事务期间 Hold 住的 GApplication（幂等）。
func (v *setupView) release() {
	if !v.held {
		return
	}
	v.held = false
	v.u.app.Release()
}

// applyProbeResult 把探测结果落成确认页。落案规则（模板预填、来源标注、
// 零写入门槛、非路径字段合并）全在 core.FinalizeProbe——CLI 的 `add` 用同一份，
// 两个视图不会对同一探测结果落出两份语义不同的档案（§6.1）。
func (v *setupView) applyProbeResult(res core.ProbeResult) {
	v.readsWarns = append([]string{}, res.ReadsWarns...)
	v.readTrace = res.ReadTrace
	// AllowEmpty 只给 GUI 首次配置：读证据不可用时允许先进入确认页由用户
	// 勾选；CLI 永不进入这条分支（§11.4）。
	p, cands, prefilled, err := v.u.env.FinalizeProbe(v.appKey, res, core.ProbeFinalize{
		Bundled:     v.bundled,
		HasBundled:  v.hasBundled,
		Existing:    v.existing,
		HasExisting: v.hasExisting,
		AllowEmpty:  v.mode == "setup" && !v.hasExisting,
	})
	if err != nil {
		// 目前唯一可能的失败是"零写入"（只有缓存/共享/凭证被写过）：重探是
		// 唯一有意义的动作，不能退化成一份什么都不隔离的空档案（§6.3）。
		v.retry = v.probe
		v.showError(err)
		return
	}
	v.profile, v.cands, v.prefilled = p, cands, prefilled
	v.u.setProfileWarns(v.appKey, p.Warnings)
	v.setState("confirm")
	v.renderConfirm()
	v.showReadsBanner()
}

// showDegraded 起降级横幅（看不到 fuse-overlayfs 时走 strace）。
// 文案不承诺完整：strace 只记录实际跑到的路径。
func (v *setupView) showDegraded() {
	v.degraded = true
	v.showReadsBanner()
}

// addReadsWarn 记一条读证据告警并立刻刷新横幅（去重，保持到达顺序）。
func (v *setupView) addReadsWarn(reason string) {
	if reason == "" || slices.Contains(v.readsWarns, reason) {
		return
	}
	v.readsWarns = append(v.readsWarns, reason)
	v.showReadsBanner()
}

// mergeWarnKeys 合并两组告警键并去重，保持先到顺序（探测告警在前，
// 候选派生告警在后）。档案里的告警与本次候选冲突可能重叠。
func mergeWarnKeys(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, w := range list {
			if w == "" || seen[w] {
				continue
			}
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// showReadsBanner 把降级原因与读证据告警拼成一条常驻横幅（§11.1：多条
// 原因不能只显示最后一条）。
func (v *setupView) showReadsBanner() {
	parts := v.readsBannerParts()
	if len(parts) == 0 {
		return
	}
	v.banner.SetTitle(strings.Join(parts, " · "))
	v.banner.SetRevealed(true)
}

// readsBannerParts 是横幅的纯文本来源，便于在无 GTK 的单测里逐态断言。
func (v *setupView) readsBannerParts() []string {
	var parts []string
	if v.degraded {
		parts = append(parts, v.u.tr("ui.setup.degraded.banner", nil))
	}
	// 四态只在确认页展示：探测中的 OnReadsWarn 也会走这里，此时 readTrace
	// 还是零值，不能把"旧档案"误报给正在探测的用户。
	if v.state == "confirm" {
		switch v.readTrace {
		case core.ReadTracePartial:
			parts = append(parts, v.u.tr("ui.setup.reads.partial", nil))
		case core.ReadTraceUnavailable:
			parts = append(parts, v.u.tr("ui.setup.reads.unavailable", nil))
		case "":
			parts = append(parts, v.u.tr("ui.setup.reads.legacy", nil))
		default:
			parts = append(parts, v.u.tr("ui.setup.reads.ok", nil))
		}
	}
	for _, w := range v.readsWarns {
		parts = append(parts, v.u.tr("probe.reads."+w, nil))
	}
	for _, w := range v.profile.Warnings {
		switch w {
		case "credential_scope_narrowed", "credential_scope_unsupported",
			"cache_credential", "cache_path_dropped", "profile_conflict",
			"credential_rows_truncated":
			parts = append(parts, v.u.tr("probe.profile."+w, nil))
		}
	}
	if len(v.profile.CredentialDropped) > 0 {
		// 与 CLI 的 `credential_scope_narrowed` 对齐：告警键只说"范围收缩"，
		// 用户还需要看到具体丢了哪条旧路径才能核对自动决策。
		parts = append(parts, v.u.tr("ui.setup.reads.dropped",
			map[string]any{"paths": strings.Join(v.profile.CredentialDropped, "、")}))
	}
	return parts
}

// retryAction 决定错误页的「重试」按钮做什么。采集线程泄漏是进程级
// 不可恢复状态：同一进程里重试会在 Probe 入口被直接拒绝，按钮等于
// 死循环，只能重启 appclone（ADR-034）。
func (v *setupView) retryAction(err error) func() {
	if errorKey(err) == "err.probe.reader_stuck" {
		return nil
	}
	return v.probe
}

func (v *setupView) showError(err error) {
	v.showErrorTitle(v.errorTitle(err), err)
}

// showProfileRejected 是「档案拒绝加载」这一态：版本超前 / 解析失败不是
// 重试能解决的问题——重跑只会读到同一份档案，重新配置/重新探测若继续探测
// 还会把它覆盖掉（§5.5）。所以这一态不挂「重试」，只留「取消」（§6.1 ③-d）。
//
// 四个入口同一规则：开启分身 / 新建分身（复用档案）、重新配置 / 重新探测
// （重定路径前要读既有档案）。
func (v *setupView) showProfileRejected(err error) {
	v.retry = nil
	title := v.u.tr("ui.setup.error.profile_unreadable", nil)
	if errorKey(err) == "err.profile.version_ahead" {
		title = v.u.tr("ui.setup.error.profile_version_ahead", nil)
	}
	v.showErrorTitle(title, err)
}

func (v *setupView) showErrorTitle(title string, err error) {
	v.errPage.SetTitle(title)
	v.errPage.SetDescription(v.describe(err))
	v.setState("error")
}

// errorTitle 按 Reason 分类（§4.5：失败态按原因分套文案）：标题说"是什么"，
// 描述（i18n）说"下一步做什么"。四种最常见的失败各有一条，其余归到通用。
func (v *setupView) errorTitle(err error) string {
	switch errorKey(err) {
	case "err.probe.app_running":
		return v.u.tr("ui.setup.error.app_running", nil)
	case "err.env.bwrap_missing":
		return v.u.tr("ui.setup.error.bwrap_missing", nil)
	case "err.lock.timeout":
		return v.u.tr("ui.setup.error.lock_timeout", nil)
	case "err.probe.no_writes":
		return v.u.tr("ui.setup.error.no_writes", nil)
	}
	return v.u.tr("ui.setup.error.generic", nil)
}

func (v *setupView) describe(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return v.u.tr(ce.Key, ce.Args)
	}
	return err.Error()
}

// renderConfirm 按候选类别分组。勾选状态只改 Candidate.Checked，
// 落点由 ProfileFromCandidates 统一决定——GUI 不自己拼 paths。
func (v *setupView) renderConfirm() {
	// 先摘掉旧页再关旧 scope：close() 会 Unref 这一轮创建的 C 对象，
	// 父容器此时还兜着一份引用，摘除与释放的顺序不能反。
	if old := v.confirmPage; old != nil {
		v.stack.Remove(&old.Widget)
	}
	if v.scope != nil {
		v.scope.close()
	}
	v.scope = &renderScope{c: v.u.cb}

	// 兜底：进度事件晚到或档案本身就是 strace 产物时，确认页仍要有那条横幅。
	if v.profile.ProbeMethod == "strace" {
		v.showDegraded()
	}
	// PreferencesPage 没有 clear()；重建一个内容页最省事，也避免旧行残留。
	pg := adw.NewPreferencesPage()
	pg.SetTitle(v.u.tr("ui.setup.confirm.title", nil))
	v.scope.own(&pg.Widget)

	// 换页 = 换一批行：旧行上登记的处理器与勾选状态都必须丢掉，
	// 否则重试一次会带着上一次的行指针去算门禁。
	v.rows = map[int]*adw.SwitchRow{}
	v.lockRow = nil
	v.dbusRow = nil
	v.runtimeRow = nil
	v.forkRow = nil

	own := adw.NewPreferencesGroup()
	v.scope.own(&own.Widget)
	own.SetTitle(v.u.tr("ui.setup.group.own", nil))
	desc := v.u.tr("ui.setup.group.own.desc.probe", nil)
	if v.mode == "open" || v.mode == "clone" {
		desc = v.u.tr("ui.setup.group.own.desc.reuse", nil)
	}
	if v.prefilled > 0 {
		desc += v.u.tr("ui.setup.group.own.prefilled", map[string]any{"n": v.prefilled})
	}
	own.SetDescription(desc)
	for i := range v.cands {
		c := v.cands[i]
		if c.Class == core.ClassShared || c.Class == core.ClassKeyring {
			continue
		}
		// 公共目录按 DisplayClass 判：既有档案里的公共路径 Class 是 Config，
		// 只看 Class 会把它渲染成能拨的开关，用户勾上后保存必然失败
		// （err.profile.public_path），门禁也会被它点亮。
		if c.DisplayClass == core.ClassPublic || c.Class == core.ClassPublic {
			row := adw.NewActionRow()
			v.scope.own(&row.Widget)
			row.SetTitle(v.u.allocText(c.Path))
			ic := v.u.symbolicIcon("folder-symbolic")
			v.scope.own(&ic.Widget)
			row.AddPrefix(&ic.Widget)
			row.SetSubtitle(v.u.tr("ui.setup.row.public.subtitle", nil))
			row.SetSensitive(false)
			own.Add(&row.Widget)
			continue
		}
		// cache / 锁定行只展示，不提供 checkbox：缓存永远不进 paths（§8.4），
		// 给一个拨不动的开关只会让用户以为"这次没生效"。
		if c.Locked || c.Class == core.ClassCache {
			row := adw.NewActionRow()
			v.scope.own(&row.Widget)
			row.SetTitle(v.u.allocText(c.Path))
			ic := v.u.symbolicIcon("folder-symbolic")
			v.scope.own(&ic.Widget)
			row.AddPrefix(&ic.Widget)
			row.SetSubtitle(v.u.tr("ui.setup.row.cache_locked", nil))
			row.SetSensitive(false)
			own.Add(&row.Widget)
			continue
		}
		row := adw.NewSwitchRow()
		v.scope.own(&row.Widget)
		row.SetTitle(v.u.allocText(c.Path))
		ic := v.u.symbolicIcon("folder-symbolic")
		v.scope.own(&ic.Widget)
		row.AddPrefix(&ic.Widget)
		d := v.u.tr(c.Why, nil)
		if c.Observed > 0 {
			d = v.u.tr("ui.setup.row.observed", map[string]any{"why": d, "n": c.Observed})
		}
		if c.ContainsCredential && c.CredentialPath != "" {
			d += " · " + v.u.tr("ui.setup.row.contains_credential",
				map[string]any{"path": v.u.allocText(c.CredentialPath)})
		}
		if c.Credential == core.CredentialStrong {
			d += " · " + v.u.tr("ui.setup.row.credential_strong", nil)
		}
		row.SetSubtitle(d)
		row.SetActive(c.Checked)
		v.rows[i] = row
		// 门禁只看「应用自身数据」的勾选数：单例行与缓存行不改变结论。
		v.scope.switchRow(row, func() { v.updateConfirmGate() })
		own.Add(&row.Widget)
	}
	if len(v.rows) == 0 {
		row := adw.NewActionRow()
		v.scope.own(&row.Widget)
		row.SetTitle(v.u.tr("ui.setup.no_paths.title", nil))
		row.SetSubtitle(v.u.tr("ui.setup.no_paths.subtitle", nil))
		row.SetSensitive(false)
		own.Add(&row.Widget)
	}
	if v.readTrace == core.ReadTraceUnavailable {
		// 降级确认页：读证据不可用，用户必须自己勾选至少一条；否则保存会被
		// 拒绝（§11.4）。把这句放在确认按钮之前，别让人点了才看到错误。
		row := adw.NewActionRow()
		v.scope.own(&row.Widget)
		row.SetTitle(v.u.tr("ui.setup.confirm.empty", nil))
		row.SetSensitive(false)
		own.Add(&row.Widget)
	}
	pg.Add(own)

	// 数据来源（§8.1）：只有真正建实例的入口才出现。「重新探测」只写档案，
	// 没有实例可承载 fork，不能给一个开了也不生效的开关。
	if v.mode != "rediscover" {
		fg := adw.NewPreferencesGroup()
		v.scope.own(&fg.Widget)
		fg.SetTitle(v.u.tr("ui.setup.group.source", nil))
		fr := adw.NewSwitchRow()
		v.scope.own(&fr.Widget)
		fr.SetTitle(v.u.tr("ui.setup.row.fork.title", nil))
		fr.SetSubtitle(v.u.tr("ui.setup.row.fork.subtitle", nil))
		fr.SetActive(v.fork)
		// 缺 fuse-overlayfs 时 fork 不会生效（run 期每个路径都降级跳过），
		// 摆一个能拨的开关等于"看起来生效、实际建了个空实例"——与 rediscover
		// 隐藏开关同一条原则。降级横幅已经解释了原因。
		fr.SetSensitive(!v.u.fuseBlocked)
		v.forkRow = fr
		v.scope.switchRow(fr, func() { v.fork = fr.GetActive() })
		fg.Add(&fr.Widget)
		pg.Add(fg)
	}

	// 单例处理：由检出的单例项驱动（锁文件 / 私有 DBus / 私有 runtime），
	// 一项都没检出就不出现——不弹一个没有开关内容的分组。
	if len(v.profile.LockFiles) > 0 || v.profile.IsolateDBus ||
		v.profile.IsolateRuntime || len(v.profile.Sockets) > 0 {
		sg := adw.NewPreferencesGroup()
		v.scope.own(&sg.Widget)
		sg.SetTitle(v.u.tr("ui.setup.group.singletons", nil))
		if len(v.profile.LockFiles) > 0 {
			lr := adw.NewSwitchRow()
			v.scope.own(&lr.Widget)
			lr.SetTitle(v.u.tr("ui.setup.row.lock.title", nil))
			lr.SetSubtitle(v.u.tr("ui.setup.row.lock.subtitle", nil))
			lr.SetActive(true)
			v.lockRow = lr
			sg.Add(&lr.Widget)
		}
		if v.profile.IsolateRuntime || len(v.profile.Sockets) > 0 {
			rr := adw.NewSwitchRow()
			v.scope.own(&rr.Widget)
			rr.SetTitle(v.u.tr("ui.setup.row.runtime.title", nil))
			rr.SetSubtitle(v.u.tr("ui.setup.row.runtime.subtitle", nil))
			rr.SetActive(v.profile.IsolateRuntime)
			v.runtimeRow = rr
			sg.Add(&rr.Widget)
		}
		dr := adw.NewSwitchRow()
		v.scope.own(&dr.Widget)
		dr.SetTitle(v.u.tr("ui.setup.row.dbus.title", nil))
		dr.SetSubtitle(v.u.tr("ui.setup.row.dbus.subtitle", nil))
		dr.SetActive(v.profile.IsolateDBus)
		v.dbusRow = dr
		sg.Add(&dr.Widget)
		pg.Add(sg)
	}

	// 公共目录：只读，永远是"没乱来"的证据。
	pub := adw.NewPreferencesGroup()
	v.scope.own(&pub.Widget)
	pub.SetTitle(v.u.tr("ui.setup.group.public", nil))
	pub.SetDescription(v.u.tr("ui.setup.group.public.desc", nil))
	prow := adw.NewActionRow()
	v.scope.own(&prow.Widget)
	prow.SetTitle(v.u.tr("ui.setup.row.public.title", nil))
	dirs := []string{}
	for _, p := range v.u.env.Resolver.PublicDirs() {
		dirs = append(dirs, v.u.allocText(p))
	}
	prow.SetSubtitle(strings.Join(dirs, " · "))
	prow.SetSubtitleLines(0)
	prow.SetSensitive(false)
	pub.Add(&prow.Widget)
	pg.Add(pub)

	// 共享但可写：必须写清代价，否则用户会把"最近文件混在一起"当成坏了。
	// 列表从**候选集**推导（而不是读档案里上次的 shared_writes）：内置模板
	// 预填进来的路径还没进档案，按档案渲染会把它们从"必须让用户看见"的组里漏掉。
	derived := v.u.env.Resolver.ProfileFromCandidates(v.appKey, v.profile.ProbeMethod, v.cands)
	if len(derived.SharedWrites) > 0 {
		sh := adw.NewPreferencesGroup()
		v.scope.own(&sh.Widget)
		sh.SetTitle(v.u.tr("ui.setup.group.shared", nil))
		sh.SetDescription(v.u.tr("ui.setup.group.shared.desc", nil))
		srow := adw.NewActionRow()
		v.scope.own(&srow.Widget)
		srow.SetTitle(v.u.tr("ui.setup.row.shared.title", nil))
		list := []string{}
		for _, p := range derived.SharedWrites {
			if abs, err := v.u.env.Resolver.Expand(p); err == nil {
				list = append(list, v.u.allocText(abs))
			}
		}
		srow.SetSubtitle(strings.Join(list, " · "))
		srow.SetSubtitleLines(0)
		srow.SetSensitive(false)
		sh.Add(&srow.Widget)
		pg.Add(sh)
	}

	// Stack 里同名子项只能有一个：新的 confirm 页在这里接上（旧页已在函数
	// 开头摘除）。
	v.confirmPage = pg
	v.stack.AddNamed(&pg.Widget, "confirm")
	v.stack.SetVisibleChildName("confirm")
	v.state = "confirm"
	v.updateConfirmGate()
}

// selectedCandidates 把用户在界面上的勾选回填进候选集。
func (v *setupView) selectedCandidates() []core.Candidate {
	out := make([]core.Candidate, len(v.cands))
	copy(out, v.cands)
	for i, row := range v.rows {
		if i < len(out) {
			out[i].Checked = row.GetActive()
		}
	}
	return out
}

func (v *setupView) buildProfile() core.Profile {
	// 界面开关先落到"生效的非路径字段"上；字段搬运与合并规则只留一份
	// （core.MergeNonPathFieldsWith），CLI 用同一份。GUI 再写一套的话，
	// 两边迟早对同一份既有档案落出不同的字段（P3，复审发现）。
	eff := v.profile
	if v.lockRow != nil && !v.lockRow.GetActive() {
		eff.LockFiles = nil
	}
	if v.dbusRow != nil {
		eff.IsolateDBus = v.dbusRow.GetActive()
	}
	if v.runtimeRow != nil {
		eff.IsolateRuntime = v.runtimeRow.GetActive()
	}
	// 落案只走 DeriveProfile（§8.1）：它负责 paths、credential_paths 的
	// 派生/收窄与告警。GUI 自己调 ProfileFromCandidates 会漏掉凭据适配。
	p := v.u.env.Resolver.DeriveProfile(eff, &eff, eff.ProbeMethod, eff.ReadTrace, v.selectedCandidates())
	// DeriveProfile 会给 ignored 填默认噪声表；复用档案（open/clone）
	// 时必须让 nil 透传：CLI 复用就是原样落盘（`p = old`），GUI 凭空补一份
	// 默认清单会把"跟随未来默认表"固化成今天这份默认值，两边字节也不一致
	// （P3，复审发现）。探测流程的 eff 自带默认表，行为不变。用 slices.Clone
	// 而不是重新补值：nil → nil，空切片（显式 `ignored = []`）→ 空切片。
	p.Ignored = slices.Clone(eff.Ignored)
	if p.Env == nil {
		p.Env = map[string]string{}
	}
	if len(p.Exec) == 0 {
		if d, err := v.u.env.LoadApp(v.appKey); err == nil {
			p.Exec, p.ExecArgs = d.Exec, d.ExecArgs
		}
	}
	return p
}

func (v *setupView) submit() {
	if v.done {
		// 页面已在退场（取消/返回）：confirmation 动画里的点击不能再建实例。
		return
	}
	if v.state == "error" {
		// 重试要重做**失败的那件事**：探测失败重探测、保存失败重保存、创建
		// 失败重创建。一律回 start() 会为一次保存失败白跑整场探测（P3，复审发现）。
		if v.retry != nil {
			v.retry()
		}
		return
	}
	if v.state != "confirm" {
		return
	}
	p := v.buildProfile()
	// 用户取消勾选会改变凭据范围；详情页应看到最终落盘版本的告警。
	v.u.setProfileWarns(v.appKey, p.Warnings)
	if len(p.Paths) == 0 {
		// 确认页允许"读证据不可用"时进入，但绝不允许落一份空档案：
		// 空 paths 的分身没有隔离任何东西（§11.4）。不挂重试——重探不会
		// 替用户做勾选。
		v.retry = nil
		v.showError(core.NewProbeNoWrites(v.appKey, len(v.cands)))
		return
	}
	if v.mode == "rediscover" {
		// 只保存档案，不建实例（GUI 详情页的「重新探测」）。
		v.saveProfile(p)
		return
	}
	v.createInstance(p)
}

// saveProfile 也是事务态：写档案要持全局写锁并按 creating 处理（ADR-022）。
// 之前这条路径直接起 goroutine 写盘——双击「保存档案」会并发两次写，
// 保存中点取消/关窗还会在页面消失之后继续写（P2，复审发现）。
func (v *setupView) saveProfile(p core.Profile) {
	if v.done {
		return
	}
	v.setState("creating")
	// 事务一旦开始就必须跑完：窗口关闭不能把写盘打断在半路（P2，复审发现）。
	// 关窗本身也由 closecb 在 creating 态挡住，Hold 是第二道保险。
	v.hold()
	go func() {
		err := func() error {
			ctx := context.Background()
			st, oerr := v.u.env.OpenState(ctx, 5*time.Second)
			if oerr != nil {
				return oerr
			}
			defer st.Close()
			if err := core.ValidateProfile(p, v.u.env.BuildOptions()); err != nil {
				return err
			}
			if err := core.ValidateCreatable(p); err != nil {
				return err
			}
			return v.u.env.SaveProfile(p)
		}()
		v.u.goIdle(func(uintptr) bool {
			v.release()
			if v.done {
				return false
			}
			if err != nil {
				// 重试只重做保存（新探测一次只会得到同一份档案）。
				v.retry = func() { v.saveProfile(p) }
				v.showErrorTitle(v.u.tr("ui.setup.error.save_failed", nil), err)
				return false
			}
			v.u.toastMsg(v.u.tr("ui.setup.toast.saved", map[string]any{"name": v.appName()}))
			v.finish(true)
			return false
		})
	}()
}

// createInstance 是创建实例的事务：持全局写锁、状态置 creating（Esc/取消/
// 返回全部挡住），成功落到详情页。
func (v *setupView) createInstance(p core.Profile) {
	if v.done {
		return
	}
	v.setState("creating")
	v.hold()
	// 在主线程快照选择：开关动画结束会补发一次 notify::active，写入 v.fork；
	// goroutine 里直接读会与该写入构成数据竞争（-race 可检出）。这份语义是
	// "提交那一刻的选择"，重试重新进 createInstance 会取到当前值。
	fork := v.fork
	go func() {
		ctx := context.Background()
		st, err := v.u.env.OpenState(ctx, 5*time.Second)
		if err == nil {
			defer st.Close()
		}
		var cerr error
		if err == nil {
			// Fork 在建实例时定死（数据层来源不是每次运行的开关）；重试沿用
			// 同一页面的选择，不会因为确认页已退场而读到零值。
			_, cerr = v.u.env.CreateInstanceWith(ctx, v.appKey, p, "", core.CreateOptions{Fork: fork})
		} else {
			cerr = err
		}
		v.u.goIdle(func(uintptr) bool {
			v.release()
			if v.done {
				return false
			}
			if cerr != nil {
				v.retry = func() { v.createInstance(p) }
				v.showErrorTitle(v.u.tr("ui.setup.error.create_failed", nil), cerr)
				return false
			}
			v.u.toastMsg(v.u.tr("ui.setup.toast.created", map[string]any{"name": v.appName()}))
			v.finish(true)
			return false
		})
	}()
}

// notifyCanceledScene：取消探测时如果 core 保留了现场（公共目录可能被写进
// upper），必须把路径告诉用户——否则文件被搬进 quarantine 却无处可寻。
// 普通取消没有现场，什么都不提示。
func (v *setupView) notifyCanceledScene(err error) {
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.canceled_kept" {
		return
	}
	args := ce.Args
	if args == nil {
		args = map[string]any{}
	}
	v.u.toastMsg(v.u.tr(ce.Key, args))
}

func (v *setupView) finish(success bool) {
	if v.done {
		return
	}
	v.done = true
	v.success = success
	if v.cancel != nil {
		v.cancel()
	}
	if v.u.activeSetup == v {
		v.u.activeSetup = nil
	}
	// 收尾（注销处理器、回调 onClose）在 popped 里做：这时页面已经真的
	// 离开导航栈，onClose 里 push 详情页不会和 pop 动画打架。
	v.u.nav.Pop()
}
