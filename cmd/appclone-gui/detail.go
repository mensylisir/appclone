package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

// detailView 是屏 2：实例列表 + 只读档案信息。运行中的实例禁止启动/删除，
// 与 CLI `rm` 的退出码 7 同源——界面不能比命令行更宽松。
type detailView struct {
	u       *ui
	appKey  string
	page    *adw.NavigationPage
	content *adw.PreferencesPage
	insts   []core.Instance
	running map[string]bool
	// stopping 记录"用户已经点过停止"的实例：runner 收尾时 launch 的 wait
	// goroutine 也会回来，此时不该再报一条「已退出」把「已停止」顶掉。
	stopping map[string]bool
	profile  core.Profile
	audits   []auditEntry
	groups   []*adw.PreferencesGroup
	// title 是 HeaderBar 里的两行标题（应用名 + 实例数），随 build 更新。
	title *adw.WindowTitle
	scope *renderScope
	// fixed 持有详情页的静态控件（工具栏、内容页），popped 时统一 Unref；
	// 导航页本身的 sink 引用由 popped 末尾的 releasePage 延后一个 idle 释放
	// （design/navspike 验证过：popped 之后 Unref 安全，不释放则整页泄漏）。
	fixed *renderScope
	sig   string

	// 详情页的数据全部在后台读（LoadProfile / ListInstances / IsRunning /
	// summary.toml）。秒级轮询若在主线程做文件 IO，实例一多界面就会卡顿。
	refreshing   bool
	pending      bool
	pendingForce bool
	seq          int
	loaded       bool
}

type auditEntry struct {
	conclusion string
	reason     string
	at         string
}

type detailData struct {
	profile    core.Profile
	profileErr error
	insts      []core.Instance
	running    map[string]bool
	audits     []auditEntry
}

func (u *ui) pushDetail(appKey string) {
	// 详情页不叠详情页：重复点同一行的箭头只刷新，不往栈里塞第二页。
	// 但只有在它**仍是可见页**时才复用：popped 之前 pointer 还在，页面却
	// 已经在离场，往它上面 render 等于点击无反应（对抗复审 P3）。
	if v := u.activeDetail; v != nil && v.appKey == appKey {
		if vis := u.nav.GetVisiblePage(); vis != nil {
			same := vis.GoPointer() == v.page.GoPointer()
			vis.Unref() // transfer full
			if same {
				v.render()
				return
			}
		}
	}
	v := &detailView{
		u: u, appKey: appKey,
		running:  map[string]bool{},
		stopping: map[string]bool{},
	}
	v.fixed = &renderScope{c: u.cb}
	v.content = adw.NewPreferencesPage()
	v.content.SetTitle(u.nameOf(appKey))
	v.fixed.own(&v.content.Widget)

	hb := adw.NewHeaderBar()
	v.fixed.own(&hb.Widget)
	v.title = adw.NewWindowTitle(u.nameOf(appKey), "")
	v.fixed.own(&v.title.Widget)
	hb.SetTitleWidget(&v.title.Widget)

	tv := adw.NewToolbarView()
	v.fixed.own(&tv.Widget)
	tv.AddTopBar(&hb.Widget)
	tv.SetContent(&v.content.Widget)
	v.page = adw.NewNavigationPage(&tv.Widget, u.nameOf(appKey))
	u.activeDetail = v
	u.pushPage(v.page, v.popped)
	v.render()
}

// popped 在详情页离开导航栈后收尾：注销处理器、清掉指针。没有这一步，
// 后台的 rename/remove 结果回来时会往一个已经不在屏幕上的页面写。
func (v *detailView) popped() {
	if v.scope != nil {
		v.scope.close()
	}
	if v.fixed != nil {
		v.fixed.close()
	}
	if v.u.activeDetail == v {
		v.u.activeDetail = nil
	}
	v.u.releasePage(v.page)
}

func (u *ui) nameOf(key string) string {
	for _, a := range u.apps {
		if a.Key == key {
			return a.Name
		}
	}
	return key
}

// render 重新读一遍数据并强制重建界面（用户动作之后调用）。
func (v *detailView) render() { v.load(true) }

// refresh 由列表轮询调用：数据照样在后台读，但只有状态真的变了才重建，
// 否则每秒重建会打断用户正在点的按钮并丢焦点。
func (v *detailView) refresh() { v.load(false) }

func (v *detailView) load(force bool) {
	if v.refreshing {
		v.pending = true
		v.pendingForce = v.pendingForce || force
		return
	}
	v.refreshing = true
	v.seq++
	seq := v.seq
	key := v.appKey
	go func() {
		d := detailData{}
		d.profile, d.profileErr = v.u.env.LoadProfile(key)
		if d.profileErr == nil {
			d.insts, _ = v.u.env.ListInstances(key)
			d.running = map[string]bool{}
			for _, in := range d.insts {
				if running, _ := v.u.env.IsRunning(key, in.ID); running {
					d.running[in.ID] = true
				}
				if c, reason, at, ok := v.u.env.CachedAuditVerdict(key, in.ID); ok {
					d.audits = append(d.audits, auditEntry{conclusion: c, reason: reason, at: at})
				}
			}
		}
		v.u.goIdle(func(uintptr) bool {
			v.refreshing = false
			force := force || v.pendingForce
			v.pendingForce = false
			if v.u.activeDetail == v && seq == v.seq {
				v.apply(d, force)
			}
			if v.pending {
				v.pending = false
				v.load(false)
			}
			return false
		})
	}()
}

func (v *detailView) apply(d detailData, force bool) {
	if d.profileErr != nil {
		msg := v.u.tr(errorKey(d.profileErr), errorArgs(d.profileErr))
		if v.loaded && v.sig == "broken:"+msg {
			return
		}
		v.sig = "broken:" + msg
		v.loaded = true
		v.buildBroken(msg)
		return
	}
	v.profile = d.profile
	v.insts = d.insts
	v.running = d.running
	v.audits = d.audits
	sig := d.sig()
	if !force && v.loaded && sig == v.sig {
		return
	}
	v.sig = sig
	v.loaded = true
	v.build()
}

func (d detailData) sig() string {
	var b strings.Builder
	for _, in := range d.insts {
		fmt.Fprintf(&b, "%s|%s|%t;", in.ID, in.DisplayName, d.running[in.ID])
	}
	for _, a := range d.audits {
		fmt.Fprintf(&b, "%s|%s|%s;", a.conclusion, a.reason, a.at)
	}
	fmt.Fprintf(&b, "|p:%d|r:%t|d:%t", len(d.profile.Paths), d.profile.IsolateRuntime, d.profile.IsolateDBus)
	return b.String()
}

// reset 摘掉旧行、注销旧处理器、建新 scope。顺序固定：先 Remove 再 close，
// close 里的 Unref 才不会踩到还挂在页上的对象。
func (v *detailView) reset() {
	for _, g := range v.groups {
		v.content.Remove(g)
	}
	v.groups = nil
	if v.scope != nil {
		v.scope.close()
	}
	v.scope = &renderScope{c: v.u.cb}
}

// buildBroken 在档案读不出来时显示一个说清原因的页面，而不是留一张
// "看起来正常、其实所有按钮都点不动"的死页（P2，复审发现）。
func (v *detailView) buildBroken(msg string) {
	v.reset()
	// 不能把 StatusPage 塞进 PreferencesGroup：Group 只接 AdwPreferencesRow，
	// 塞进去会被裁成半截图标（列表空态踩过同一个坑）。这里用带警告图标的
	// 只读行承载原因，信息一样完整。
	g := adw.NewPreferencesGroup()
	v.scope.own(&g.Widget)
	g.SetTitle(v.u.tr("ui.detail.profile_broken.title", nil))
	g.SetDescription(msg)
	row := adw.NewActionRow()
	v.scope.own(&row.Widget)
	row.SetTitle(v.u.tr("ui.detail.profile_broken.row", nil))
	row.SetSubtitle(v.u.tr("ui.detail.profile_broken.hint", nil))
	row.SetSubtitleLines(0)
	v.prefix(row, "dialog-warning-symbolic")
	row.SetSensitive(false)
	g.Add(&row.Widget)
	v.content.Add(g)
	v.groups = append(v.groups, g)
}

// prefix 给只读行加一个 16px 符号图标，让密集的档案信息有视觉锚点。
func (v *detailView) prefix(row *adw.ActionRow, icon string) {
	ic := v.u.symbolicIcon(icon)
	v.scope.own(&ic.Widget)
	row.AddPrefix(&ic.Widget)
}

// instanceSummary 是详情页标题的第二行：实例数与运行中数量。
func (v *detailView) instanceSummary(insts []core.Instance, running map[string]bool) string {
	if len(insts) == 0 {
		return v.u.tr("ui.detail.clones.none", nil)
	}
	n := 0
	for _, in := range insts {
		if running[in.ID] {
			n++
		}
	}
	if n > 0 {
		return v.u.tr("ui.detail.clones.count_running", map[string]any{"n": len(insts), "running": n})
	}
	return v.u.tr("ui.detail.clones.count", map[string]any{"n": len(insts)})
}

func (v *detailView) build() {
	v.reset()
	p := v.profile
	if v.title != nil {
		v.title.SetSubtitle(v.instanceSummary(v.insts, v.running))
	}

	// ---- 实例 ----
	ig := adw.NewPreferencesGroup()
	v.scope.own(&ig.Widget)
	ig.SetTitle(v.u.tr("ui.detail.group.instances", nil))
	ig.SetDescription(v.u.tr("ui.detail.group.instances.desc", nil))
	for _, in := range v.insts {
		running := v.running[in.ID]
		row := adw.NewActionRow()
		v.scope.own(&row.Widget)
		row.SetTitle(in.DisplayName)
		ic := v.u.symbolicIcon("edit-copy-symbolic")
		v.scope.own(&ic.Widget)
		row.AddPrefix(&ic.Widget)
		state := v.u.tr("ui.instance.stopped", nil)
		if running {
			state = v.u.tr("ui.instance.running", nil)
		}
		sub := state
		if in.LastUsedAt != nil {
			sub = v.u.tr("ui.instance.last_used", map[string]any{
				"state": state,
				"time":  in.LastUsedAt.Local().Format("01-02 15:04"),
			})
		}
		row.SetSubtitle(sub)

		box := gtk.NewBox(gtk.OrientationHorizontalValue, 6)
		v.scope.own(&box.Widget)
		box.SetValign(gtk.AlignCenterValue)

		inst := in
		// 运行中把「启动」换成「停止」：隐藏一个点不动的按钮比换动作更诚实，
		// 同时保留"这个分身能做什么"的可见性（ADR-028）。
		if running {
			stop, stopC := v.u.iconButton("media-playback-stop-symbolic", v.u.tr("ui.instance.stop", nil))
			v.scope.own(&stop.Widget)
			v.scope.own(&stopC.Widget)
			stop.AddCssClass("destructive-action")
			stop.SetTooltipText(v.u.tr("ui.instance.stop.tooltip", nil))
			v.scope.button(stop, func() { v.stop(inst) })
			box.Append(&stop.Widget)
		} else {
			launch, launchC := v.u.iconButton("media-playback-start-symbolic", v.u.tr("ui.instance.launch", nil))
			v.scope.own(&launch.Widget)
			v.scope.own(&launchC.Widget)
			launch.AddCssClass("suggested-action")
			launch.SetTooltipText(v.u.tr("ui.instance.launch.tooltip", nil))
			v.scope.button(launch, func() { v.launch(inst) })
			box.Append(&launch.Widget)
		}

		rename, renameC := v.u.iconButton("document-edit-symbolic", v.u.tr("ui.instance.rename", nil))
		v.scope.own(&rename.Widget)
		v.scope.own(&renameC.Widget)
		rename.SetSensitive(!running)

		del, delC := v.u.iconButton("user-trash-symbolic", v.u.tr("ui.instance.delete", nil))
		v.scope.own(&del.Widget)
		v.scope.own(&delC.Widget)
		del.AddCssClass("destructive-action")
		del.SetSensitive(!running)
		if running {
			// 按钮不可点时提示要解释"为什么"，否则用户会以为删除坏了。
			del.SetTooltipText(v.u.tr("ui.instance.delete.tooltip.running", nil))
		} else {
			del.SetTooltipText(v.u.tr("ui.instance.delete.tooltip", nil))
		}

		v.scope.button(rename, func() { v.rename(inst) })
		v.scope.button(del, func() { v.remove(inst) })

		box.Append(&rename.Widget)
		box.Append(&del.Widget)
		row.AddSuffix(&box.Widget)
		ig.Add(&row.Widget)
	}
	if len(v.insts) == 0 {
		row := adw.NewActionRow()
		v.scope.own(&row.Widget)
		row.SetTitle(v.u.tr("ui.detail.instances.empty", nil))
		row.SetSensitive(false)
		ig.Add(&row.Widget)
	}
	add := adw.NewActionRow()
	v.scope.own(&add.Widget)
	add.SetTitle(v.u.tr("ui.detail.new_clone", nil))
	add.SetSubtitle(v.u.tr("ui.detail.new_clone.subtitle", nil))
	addIc := v.u.symbolicIcon("list-add-symbolic")
	v.scope.own(&addIc.Widget)
	add.AddPrefix(&addIc.Widget)
	add.SetActivatable(true)
	v.scope.row(add, func() {
		mode := "clone"
		if !v.profileIsCreatable() {
			mode = "reconfig"
		}
		v.u.pushSetup(v.appKey, mode, func(ok bool) {
			if ok && v.u.activeDetail == v {
				v.render()
			}
		})
	})
	ig.Add(&add.Widget)
	v.content.Add(ig)
	v.groups = append(v.groups, ig)

	// ---- 档案（只读）----
	pg := adw.NewPreferencesGroup()
	v.scope.own(&pg.Widget)
	pg.SetTitle(v.u.tr("ui.detail.settings", nil))
	pg.SetDescription(v.u.tr("ui.detail.settings.desc", nil))

	paths := adw.NewActionRow()
	v.scope.own(&paths.Widget)
	paths.SetTitle(v.u.tr("ui.detail.paths", nil))
	paths.SetSubtitle(v.u.tr("ui.detail.paths.count", map[string]any{"n": len(p.Paths)}))
	v.prefix(paths, "folder-symbolic")
	list := make([]string, 0, len(p.Paths))
	for _, raw := range p.Paths {
		if abs, err := v.u.env.Resolver.Expand(raw); err == nil {
			list = append(list, v.u.allocText(abs))
		}
	}
	paths.SetSubtitleLines(0)
	paths.SetSensitive(false)
	pg.Add(&paths.Widget)
	// 路径明细单独一行、排在标题行**之后**：AdwPreferencesGroup 是按 Add 顺序
	// 排的，先加明细会让它跑到「隔离路径」上面，读起来像一行没头没尾的字。
	if len(list) > 0 {
		extra := adw.NewActionRow()
		v.scope.own(&extra.Widget)
		extra.SetTitle("")
		extra.SetSubtitle(strings.Join(list, " · "))
		extra.SetSubtitleLines(0)
		extra.SetSensitive(false)
		pg.Add(&extra.Widget)
	}

	conf := adw.NewActionRow()
	v.scope.own(&conf.Widget)
	conf.SetTitle(v.u.tr("ui.detail.probe_method", nil))
	if p.ProbeMethod == "strace" {
		conf.SetSubtitle(v.u.tr("ui.detail.probe.strace", nil))
	} else if p.ProbeMethod == "manual" || p.ProbeMethod == "bundled" {
		conf.SetSubtitle(v.u.tr("ui.detail.probe.bundled", nil))
	} else {
		conf.SetSubtitle(v.u.tr("ui.detail.probe.overlay", nil))
	}
	v.prefix(conf, "system-search-symbolic")
	conf.SetSensitive(false)
	pg.Add(&conf.Widget)

	if len(p.SharedWrites) > 0 {
		sh := adw.NewActionRow()
		v.scope.own(&sh.Widget)
		sh.SetTitle(v.u.tr("ui.detail.shared", nil))
		sl := []string{}
		for _, raw := range p.SharedWrites {
			if abs, err := v.u.env.Resolver.Expand(raw); err == nil {
				sl = append(sl, v.u.allocText(abs))
			}
		}
		sh.SetSubtitle(v.u.tr("ui.detail.shared.subtitle", map[string]any{"list": strings.Join(sl, " · ")}))
		sh.SetSubtitleLines(0)
		v.prefix(sh, "folder-publicshare-symbolic")
		sh.SetSensitive(false)
		pg.Add(&sh.Widget)
	}

	single := adw.NewActionRow()
	v.scope.own(&single.Widget)
	single.SetTitle(v.u.tr("ui.detail.single", nil))
	var parts []string
	if len(p.LockFiles) > 0 {
		parts = append(parts, v.u.tr("ui.detail.single.lock", nil))
	}
	if p.IsolateRuntime {
		parts = append(parts, v.u.tr("ui.detail.single.runtime_private", nil))
	} else {
		parts = append(parts, v.u.tr("ui.detail.single.runtime_shared", nil))
	}
	if p.IsolateDBus {
		parts = append(parts, v.u.tr("ui.detail.single.dbus_private", nil))
	} else {
		parts = append(parts, v.u.tr("ui.detail.single.dbus_shared", nil))
	}
	single.SetSubtitle(strings.Join(parts, " · "))
	v.prefix(single, "system-lock-screen-symbolic")
	single.SetSensitive(false)
	pg.Add(&single.Widget)

	audit := adw.NewActionRow()
	v.scope.own(&audit.Widget)
	audit.SetTitle(v.u.tr("ui.detail.audit", nil))
	if e, ok := v.latestAudit(); ok {
		audit.SetSubtitle(v.auditText(e))
	} else {
		audit.SetSubtitle(v.u.tr("ui.detail.audit.no_evidence", nil))
	}
	v.prefix(audit, "security-high-symbolic")
	audit.SetSensitive(false)
	pg.Add(&audit.Widget)

	rootRow := adw.NewActionRow()
	v.scope.own(&rootRow.Widget)
	rootRow.SetTitle(v.u.tr("ui.detail.root", nil))
	if len(v.insts) > 0 {
		rootRow.SetSubtitle(v.u.allocText(v.insts[0].Root))
	} else {
		rootRow.SetSubtitle("—")
	}
	v.prefix(rootRow, "folder-symbolic")
	rootRow.SetSensitive(false)
	pg.Add(&rootRow.Widget)

	v.content.Add(pg)
	v.groups = append(v.groups, pg)

	// 重新探测：只更新档案，不建实例（GUI 与 CLI --profile-only 同口径）。
	rg := adw.NewPreferencesGroup()
	v.scope.own(&rg.Widget)
	redisc := adw.NewActionRow()
	v.scope.own(&redisc.Widget)
	redisc.SetTitle(v.u.tr("ui.detail.redisc", nil))
	redisc.SetSubtitle(v.u.tr("ui.detail.redisc.subtitle", nil))
	v.prefix(redisc, "view-refresh-symbolic")
	redisc.SetActivatable(true)
	v.scope.row(redisc, func() {
		v.u.pushSetup(v.appKey, "rediscover", func(ok bool) {
			if ok && v.u.activeDetail == v {
				v.render()
			}
		})
	})
	rg.Add(&redisc.Widget)
	v.content.Add(rg)
	v.groups = append(v.groups, rg)
}

// latestAudit 取该应用所有实例里最新的一份已存审计结论。
// 只读 summary，不触发 finalize——详情页看一眼证据不该把证据消费掉。
func (v *detailView) latestAudit() (auditEntry, bool) {
	var best auditEntry
	ok := false
	for _, e := range v.audits {
		if !ok || e.at > best.at {
			best, ok = e, true
		}
	}
	return best, ok
}

// auditText 按 conclusion + reason 说人话。
//
// `unverified` 不是一种状态而是四种：没开审计 / 证据没写完 / 确实没观测到
// 写入 / 证据读不出来。只显示"没有给出结论"会把"证据坏了"藏成"没结论"，
// 用户据此会去重跑，而不是去修文件。
func (v *detailView) auditText(e auditEntry) string {
	var text string
	switch e.conclusion {
	case core.VerdictVerified:
		text = v.u.tr("ui.audit.verified", nil)
	case core.VerdictLeak:
		text = v.u.tr("ui.audit.leak", nil)
	case core.VerdictUnverified:
		switch e.reason {
		case "audit_not_enabled":
			text = v.u.tr("ui.audit.unverified.none", nil)
		case "audit_incomplete":
			text = v.u.tr("ui.audit.unverified.incomplete", nil)
		case "no_writes":
			text = v.u.tr("ui.audit.unverified.no_writes", nil)
		case "audit_unreadable":
			text = v.u.tr("ui.audit.unverified.corrupt", nil)
		case "extract_failed":
			text = v.u.tr("ui.audit.unverified.public_extract", nil)
		default:
			text = v.u.tr("ui.audit.unverified.unknown", nil)
		}
	default:
		text = v.u.tr("ui.audit.stored", nil)
	}
	if e.at != "" {
		text += " · " + e.at
	}
	return text
}

func (v *detailView) profileIsCreatable() bool {
	has, creatable, err := v.u.env.ProfileState(v.appKey)
	return err == nil && has && creatable
}

func (v *detailView) launch(in core.Instance) {
	// ADR-028：不在 GUI 进程里跑 run，而是 detached runner；关窗口不再
	// 连带结束应用。run.log 是启动失败时唯一的线索。
	wait, logPath, logOffset, err := startDetached(v.u.env, v.appKey, in.ID)
	if err != nil {
		// 底层错误（权限、ENOENT）对普通用户没有行动价值，不进 toast。
		v.u.toastMsg(v.u.tr("ui.toast.launch_failed", map[string]any{
			"reason": v.u.tr("ui.toast.launch_failed.start", nil),
		}))
		return
	}
	v.u.toastMsg(v.u.tr("ui.toast.launching", map[string]any{"name": in.DisplayName}))
	go func() {
		start := time.Now()
		werr := wait()
		// 2 秒内就退出的 runner 几乎一定是启动失败（bwrap 缺失、实例已在
		// 运行、档案坏了）；跑起来的应用不会这么快退出，晚退出的报"已退出"。
		v.notifyRunEnded(in, werr, time.Since(start) < 2*time.Second, logPath, logOffset)
	}()
}

// notifyRunEnded 只在主循环里改界面：runner 的退出可能发生在页面已经
// pop 掉之后，此时只刷新列表，不往旧页面写。
func (v *detailView) notifyRunEnded(in core.Instance, werr error, early bool, logPath string, logOffset int64) {
	v.u.goIdle(func(uintptr) bool {
		if v.stopping[in.ID] {
			// 用户点的停止：结果由 stop() 自己报（成功「已停止」/失败错误），
			// 这里再报「已退出」既与交互基线不符，也会把真正的通知顶掉。
			delete(v.stopping, in.ID)
		} else if werr != nil && early {
			reason := lastLogLine(logPath, logOffset)
			if reason == "" {
				reason = v.u.tr("ui.toast.launch_failed.generic", nil)
			}
			v.u.toastMsg(v.u.tr("ui.toast.launch_failed", map[string]any{"reason": reason}))
		} else {
			v.u.toastMsg(v.u.tr("ui.toast.instance_exited", map[string]any{"name": in.DisplayName}))
		}
		v.u.reload()
		if v.u.activeDetail == v {
			v.render()
		}
		return false
	})
}

func (v *detailView) stop(in core.Instance) {
	// 停止会结束应用：未保存的内容可能丢，必须二次确认，默认「取消」。
	d := adw.NewAlertDialog(
		v.u.tr("ui.dialog.stop.title", map[string]any{"name": in.DisplayName}),
		v.u.tr("ui.dialog.stop.body", nil))
	d.AddResponse("cancel", v.u.tr("ui.dialog.cancel", nil))
	d.AddResponse("stop", v.u.tr("ui.dialog.stop.action", nil))
	d.SetDefaultResponse("cancel")
	d.SetResponseAppearance("stop", adw.ResponseDestructiveValue)
	v.u.cb.onDialog(d, func(resp string) {
		if resp != "stop" {
			return
		}
		// 先记账再发信号：runner 可能比 StopRun 的轮询更快退出，wait
		// goroutine 回来时要知道这条「已退出」是用户自己点出来的。
		v.stopping[in.ID] = true
		v.u.toastMsg(v.u.tr("ui.toast.stopping", map[string]any{"name": in.DisplayName}))
		go func() {
			// 6 秒的组终止链 + 收尾余量；StopRun 自己也有 6 秒上限。
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			err := v.u.env.StopRun(ctx, v.appKey, in.ID)
			v.u.goIdle(func(uintptr) bool {
				if err != nil {
					// 停止没成功：清掉记账，下一次真正的退出仍要告诉用户。
					delete(v.stopping, in.ID)
					v.u.toastMsg(v.u.tr(errorKey(err), errorArgs(err)))
				} else {
					v.u.toastMsg(v.u.tr("ui.toast.stopped", map[string]any{"name": in.DisplayName}))
				}
				v.u.reload()
				if v.u.activeDetail == v {
					v.render()
				}
				return false
			})
		}()
	})
	d.Choose(&v.u.win.Widget, nil, nil, 0)
}

func (v *detailView) rename(in core.Instance) {
	entry := gtk.NewEntry()
	entry.SetText(in.DisplayName)
	d := adw.NewAlertDialog(v.u.tr("ui.dialog.rename.title", nil), v.u.tr("ui.dialog.rename.body", nil))
	d.SetExtraChild(&entry.Widget)
	d.AddResponse("cancel", v.u.tr("ui.dialog.cancel", nil))
	d.AddResponse("rename", v.u.tr("ui.dialog.save", nil))
	d.SetDefaultResponse("cancel")
	d.SetResponseAppearance("rename", adw.ResponseSuggestedValue)
	v.u.cb.onDialog(d, func(resp string) {
		if resp != "rename" {
			return
		}
		name := strings.TrimSpace(entry.GetText())
		if name == "" || name == in.DisplayName {
			return
		}
		go func() {
			// 重命名写 instance.toml，是命名空间变更：与 CLI 的 add/rm 一样
			// 必须持全局写锁（ADR-022），否则可能与并发的创建/删除互相覆盖。
			err := func() error {
				ctx := context.Background()
				st, oerr := v.u.env.OpenState(ctx, 5*time.Second)
				if oerr != nil {
					return oerr
				}
				defer st.Close()
				return v.u.env.RenameInstance(v.appKey, in.ID, name)
			}()
			v.u.goIdle(func(uintptr) bool {
				if err != nil {
					v.u.toastMsg(v.u.tr(errorKey(err), errorArgs(err)))
					return false
				}
				v.u.toastMsg(v.u.tr("ui.toast.renamed", map[string]any{"name": name}))
				// 详情页可能已经被 pop 掉：此时只刷新列表，不往旧页面写。
				if v.u.activeDetail == v {
					v.render()
				}
				v.u.reload()
				return false
			})
		}()
	})
	d.Choose(&v.u.win.Widget, nil, nil, 0)
}

func (v *detailView) remove(in core.Instance) {
	// 删除一律走回收站；默认响应是「取消」——回车不能完成破坏性操作。
	d := adw.NewAlertDialog(v.u.tr("ui.dialog.delete.title", map[string]any{"name": in.DisplayName}), v.u.tr("ui.dialog.delete.body", nil))
	d.AddResponse("cancel", v.u.tr("ui.dialog.cancel", nil))
	d.AddResponse("trash", v.u.tr("ui.dialog.trash", nil))
	d.SetDefaultResponse("cancel")
	d.SetResponseAppearance("trash", adw.ResponseDestructiveValue)
	v.u.cb.onDialog(d, func(resp string) {
		if resp != "trash" {
			return
		}
		// 删除是事务：Hold 住进程并挡住关窗，别把 gio trash 打断在半路。
		v.u.beginBusy()
		go func() {
			ctx := context.Background()
			st, err := v.u.env.OpenState(ctx, 5*time.Second)
			if err == nil {
				defer st.Close()
			}
			var derr error
			if err == nil {
				derr = v.u.env.DeleteInstance(ctx, v.appKey, in.ID)
			} else {
				derr = err
			}
			v.u.goIdle(func(uintptr) bool {
				v.u.endBusy()
				if derr != nil {
					v.u.toastMsg(v.u.tr(errorKey(derr), errorArgs(derr)))
					v.u.reload()
					return false
				}
				v.u.toastMsg(v.u.tr("ui.toast.trashed", map[string]any{"name": in.DisplayName}))
				// 页面可能已经不在栈顶（用户返回了）：只在仍是当前详情页时才动它。
				if v.u.activeDetail == v {
					if len(v.insts) <= 1 {
						// 最后一个实例没了：回列表，详情页指向的东西已经不存在。
						v.popIfVisible()
					} else {
						v.render()
					}
				}
				v.u.reload()
				return false
			})
		}()
	})
	d.Choose(&v.u.win.Widget, nil, nil, 0)
}

// popIfVisible 只在详情页确实是屏幕上那一页时才 pop。
//
// 删除事务在后台跑，期间用户可能已经用返回按钮离开（页面正在 pop 动画里，
// 但 activeDetail 还没在 popped 里清掉）。此时无条件 nav.Pop() 会连带把
// 列表页弹掉——用户看到的是窗口突然空了一屏（P3，复审发现）。
// GetVisiblePage 是 transfer full（puregotk 已 IncreaseRef），查完必须 Unref。
func (v *detailView) popIfVisible() {
	vis := v.u.nav.GetVisiblePage()
	if vis == nil {
		return
	}
	ptr := vis.GoPointer()
	vis.Unref()
	if ptr == v.page.GoPointer() {
		v.u.nav.Pop()
	}
	// 已经不是可见页：页面正在离开导航栈，popped 会照常收尾；往它上面
	// 重画只会给一张即将销毁的页写数据。
}

// errorKey/errorArgs 从 core.Error 提取 i18n 键；非 core 错误落到通用文案。
func errorKey(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Key
	}
	return "err.internal.unknown"
}

func errorArgs(err error) map[string]any {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Args
	}
	return map[string]any{"message": err.Error()}
}
