package main

import (
	"sync"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"
)

// 回调纪律（P0，2026-11-08 复审发现）
//
// purego 给每个 *func 变量地址在进程内**永久**占一个 C 回调槽，上限 2000
// （purego/syscall_sysv.go 的 maxCB）。在渲染路径里"每次新建一个闭包变量再
// Connect"——即使界面上只看得到几十个控件——每小时就会耗尽，随后整个进程
// panic "purego: the maximum number of callbacks has been reached"。
// 这不是内存泄漏，是运行一小时后必崩。
//
// 因此渲染路径只登记**处理器**（纯 Go，随页面重建自由增删），C 槽只由下面
// 五个稳定回调占用：它们是 callbacks 的字段，地址在进程内恒定，注册一次后
// 永不重新赋值。
//
// 这里容易写反的一个事实：puregotk 的 Connect* 以**变量地址**为键缓存
// trampoline，而 trampoline 触发时才解引用该地址（`cbFn := *cb`）。所以
// （a）变量必须一直存活——被回收就是读已释放内存（见 ui.hold）；
// （b）给字段重新赋值会同时改掉所有已连接信号的行为，而不是"新连接才生效"。
// 字段一次赋值后不再改动，正是为了让 (b) 永远不是一个隐患。
type callbacks struct {
	notifyFn  func(gobject.Object, uintptr) // notify::active（开关行）
	clickedFn func(gtk.Button)              // clicked（按钮）
	rowFn     func(adw.ActionRow)           // activated（可点行）
	dialogFn  func(adw.AlertDialog, string) // response（对话框）
	idleFn    glib.SourceFunc               // 从后台线程切回主线程

	mu      sync.Mutex
	switchN map[uintptr]func()             // 开关行指针 → 处理器
	action  map[uintptr]func()             // 按钮/可点行指针 → 处理器
	dialog  map[uintptr]func(string)       // 对话框指针 → 处理器
	idle    map[uintptr]func(uintptr) bool // idle 序号 → 任务
	idleSeq uintptr
}

func newCallbacks() *callbacks {
	c := &callbacks{
		switchN: map[uintptr]func(){},
		action:  map[uintptr]func(){},
		dialog:  map[uintptr]func(string){},
		idle:    map[uintptr]func(uintptr) bool{},
	}
	c.notifyFn = func(obj gobject.Object, pspec uintptr) {
		// 只认 active：行在挂载/换父时会发 parent、root 等无关 notify，
		// 全部交给 onSwitch 等于把一个隐式不变量押在"值恰好没变"上
		// （对抗性复审 P3）。动画结束的补发仍是 active，由 ui.toggled 去重。
		if pspec != 0 && gobject.ParamSpecNewFromInternalPtr(pspec).GetName() != "active" {
			return
		}
		if fn := c.switchHandler(obj.GoPointer()); fn != nil {
			fn()
		}
	}
	c.clickedFn = func(b gtk.Button) {
		if fn := c.actionHandler(b.GoPointer()); fn != nil {
			fn()
		}
	}
	c.rowFn = func(r adw.ActionRow) {
		if fn := c.actionHandler(r.GoPointer()); fn != nil {
			fn()
		}
	}
	c.dialogFn = func(d adw.AlertDialog, resp string) {
		ptr := d.GoPointer()
		c.mu.Lock()
		fn := c.dialog[ptr]
		delete(c.dialog, ptr)
		c.mu.Unlock()
		if fn != nil {
			fn(resp)
		}
	}
	c.idleFn = func(id uintptr) bool {
		// 先摘掉再执行：任务里再排一个 idle 是正常用法，不能互相顶掉。
		c.mu.Lock()
		fn := c.idle[id]
		delete(c.idle, id)
		c.mu.Unlock()
		if fn == nil {
			return false
		}
		return fn(id)
	}
	return c
}

func (c *callbacks) switchHandler(ptr uintptr) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.switchN[ptr]
}

func (c *callbacks) actionHandler(ptr uintptr) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.action[ptr]
}

// idleAdd 可以从任意后台线程调用：任务表自带锁，C 槽由 idleFn 独占。
func (c *callbacks) idleAdd(fn func(uintptr) bool) {
	c.mu.Lock()
	c.idleSeq++
	id := c.idleSeq
	c.idle[id] = fn
	c.mu.Unlock()
	glib.IdleAdd(&c.idleFn, id)
}

// onDialog 登记并连接一个对话框的响应处理器。响应到达即注销。
//
// 「登记即连接」是刻意的：处理器表只负责分派，信号必须真的接上——只登记
// 不 Connect 的写法在界面上表现为"按钮点了没反应"，而且不会报任何错。
func (c *callbacks) onDialog(d *adw.AlertDialog, fn func(string)) {
	ptr := d.GoPointer()
	c.mu.Lock()
	// 正常路径上同时只有一个对话框；表异常长说明有走丢的条目（对话框没走
	// response 就消失），整体清掉比无限增长安全——那些条目本来也不会再被触发。
	if len(c.dialog) >= 64 {
		c.dialog = map[uintptr]func(string){}
	}
	c.dialog[ptr] = fn
	c.mu.Unlock()
	d.ConnectResponse(&c.dialogFn)
}

func (c *callbacks) onAction(ptr uintptr, fn func()) {
	c.mu.Lock()
	c.action[ptr] = fn
	c.mu.Unlock()
}

func (c *callbacks) watchSwitch(ptr uintptr, fn func()) {
	c.mu.Lock()
	c.switchN[ptr] = fn
	c.mu.Unlock()
}

// drop 注销一批处理器。**页面重建时必须调用**：处理器闭包持有旧控件与
// 旧应用数据，留在表里既泄漏，也可能在 GObject 地址被复用时派给错误的控件。
func (c *callbacks) drop(keys []uintptr) {
	if len(keys) == 0 {
		return
	}
	c.mu.Lock()
	for _, k := range keys {
		delete(c.switchN, k)
		delete(c.action, k)
	}
	c.mu.Unlock()
}

// renderScope 收集一轮渲染登记的全部处理器与 C 对象，重建时整体注销/释放。
// 每个视图（列表 / 详情 / 确认页）各持一个，互不牵连。
//
// 三个登记方法都会顺手把信号接到 callbacks 的稳定入口上：只登记不连接，
// 界面上就是"点了没反应"，且没有任何报错可查。
type renderScope struct {
	c     *callbacks
	keys  []uintptr
	owned []unrefable
}

// unrefable 是 gtk.Widget 及其派生类（通过嵌入的 gobject.InitiallyUnowned）
// 都满足的最小接口。
type unrefable interface{ Unref() }

// own 记住本轮渲染创建的 C 对象。puregotk 的构造器对每个对象 IncreaseRef，
// 而包装没有 finalizer——从父容器 Remove 只释放父容器那一份引用，构造器给
// 我们这一份必须自己 Unref。不这样做，搜索每敲一个字重建 130 行，旧行就
// 会永久存活（Go 堆的 keep 不涨了，C 侧内存照涨）。
func (s *renderScope) own(o unrefable) {
	if o != nil {
		s.owned = append(s.owned, o)
	}
}

func (s *renderScope) switchRow(row *adw.SwitchRow, fn func()) {
	ptr := row.GoPointer()
	s.c.watchSwitch(ptr, fn)
	s.keys = append(s.keys, ptr)
	row.ConnectNotify(&s.c.notifyFn)
}

func (s *renderScope) button(b *gtk.Button, fn func()) {
	ptr := b.GoPointer()
	s.c.onAction(ptr, fn)
	s.keys = append(s.keys, ptr)
	b.ConnectClicked(&s.c.clickedFn)
}

func (s *renderScope) row(r *adw.ActionRow, fn func()) {
	ptr := r.GoPointer()
	s.c.onAction(ptr, fn)
	s.keys = append(s.keys, ptr)
	r.ConnectActivated(&s.c.rowFn)
}

func (s *renderScope) close() {
	s.c.drop(s.keys)
	s.keys = nil
	// 逆序释放：先子后父，避免容器先被释放后还要访问它的孩子。
	for i := len(s.owned) - 1; i >= 0; i-- {
		s.owned[i].Unref()
	}
	s.owned = nil
}
