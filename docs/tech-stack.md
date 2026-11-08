# appclone 技术选型

> 本文记录 appclone 产品（Go + GUI，直达用户）的完整技术选型决策，
> 以及每项决策的理由与被否决的备选方案。与[方案设计](方案设计.md)配套阅读。
> 早期 bash 原型仅用于验证隔离语义，已归档 `legacy/`，不属于产品路径（ADR-002）。

## 0. 选型总览

| 层 | 选型 | 定位 |
|---|---|---|
| 隔离引擎 | bubblewrap (bwrap) | mount namespace 沙箱，唯一核心依赖 |
| 路径探测 | overlay-diff（fuse-overlayfs）；strace 兜底 | 自动摸清应用写入路径（ADR-008） |
| 克隆起步 | fuse-overlayfs | 无 root 的 copy-on-write，分身继承主实例数据 |
| 桌面集成 | .desktop 标准 + gio trash | 图标入口 / 回收站删除 |
| 核心层 | Go ≥ 1.22，package `core` | 探测管线、档案解析、实例管理、bwrap 参数构造 |
| CLI | cobra | `cmd/appclone`，开发者视图 |
| GUI | GTK4 + libadwaita，经 **puregotk** 绑定 | `cmd/appclone-gui`，普通用户主入口（ADR-005 二次修订） |
| 档案格式 | TOML | 结构化 per-app 路径知识库 |
| 构建/分发 | Go 单二进制 + nfpm 出 deb/rpm/Arch 包 | 全仓 `CGO_ENABLED=0`，amd64/arm64 交叉编译成立；**仅 Linux**（ADR-016） |
| CI | GitHub Actions（lint + test + build） | gofmt/vet/staticcheck + go test |

## 1. 隔离引擎：bubblewrap

### 1.1 为什么是它

- **专职单一职责**：只做 mount namespace 隔离，参数即行为（`--bind`/`--ro-bind`/`--dev-bind`），无隐藏语义，可被程序精确构造——这是"数据目录偷换、其余全透传"语义的最小实现；
- **无特权设计**：SUID-free，依赖非特权 user namespace，与"无需 root"的项目目标严格一致；
- **无 daemon、无劫持**：不拦截系统调用（对比 ptrace 系方案），无性能损耗、无兼容性玄学；
- **经过量产验证**：Flatpak 的官方沙箱引擎，被所有主流发行版长期维护。

### 1.2 被否决的备选

| 备选 | 否决原因 |
|---|---|
| firejail | 功能全但历史安全漏洞多、复杂度高；本项目只需要它的 profile 子集，不值得引入其攻击面 |
| systemd-run (BindPaths) | 原理等价，适合临时调试；不适合产品化（强绑定 systemd 会话、参数难以编程组合、无分发故事） |
| rootless podman | 引入完整容器 runtime（storage/CNI 体系）只为换几个目录，纯负担且偏离"轻量"定位 |
| proot / ptrace 系 | 用户态拦截系统调用，性能差、兼容性差——相当于回到 Android "虚拟化框架"路线的缺陷 |

### 1.3 配套组件（每项唯一合理选择）

- **strace**：无侵入发现应用真实写入路径的唯一可靠手段（`-e trace=%file` + 写标志过滤）；
- **fuse-overlayfs**：无 root 下唯一的 CoW 目录方案，支撑"分身从主实例数据克隆起步"；
- **gio trash / freedesktop .desktop**：桌面生态标准，无需自造。

## 2. 实现语言：Go（唯一实现，产品直达）

### 2.1 决策：不存在 bash v1

项目目标是**面向普通用户的产品**——产品标准是"像安卓设置里一个开关"。
bash 原型（仓库早期根目录脚本）无法承载：探测自动化、discover 勾选交互、
结构化错误、多发行版分发。任何"先发 bash 版收反馈"的路线都不成立：
让用户开终端、手填路径、读 strace 输出的工具没有用户。

bash 原型已完成它的历史使命：验证了 bwrap 绑定语义与目录布局，
其 `build_bwrap_args` 逻辑由 Go `core.BuildBwrapArgs` 接管，脚本本身移出
产品路径（归档到 `legacy/`，仅供 core 开发期做行为对照，不发布、不承诺维护）。

### 2.2 为什么是 Go 而非 Rust / Python

| 候选 | 判定 |
|---|---|
| **Go** | 瓶颈全在 bwrap 与应用本身，"薄封装"场景无极致性能需求；单静态二进制 + 交叉编译 + 成熟 `os/exec`/sysadmin 生态，与"封装 bwrap"的定位完全匹配 |
| Rust | 安全收益在此薄封装场景无法兑现，开发成本高，CLI/GUI 双视图迭代速度慢于 Go |
| Python | 部署是死穴：目标用户机器上解释器/依赖不可控，与"工具必须开箱即用"冲突 |
| C（GTK 的原生语言） | 省掉绑定层，但把 core 的解析、并发、单测、分发成本成倍抬高；GTK 是 C 不等于应用逻辑要用 C，详见 §2.3 |

**一条红线**：bwrap 参数构造逻辑（`core` 包中的 `BuildBwrapArgs`）是本项目
的事实核心——它是 Android PMS"安装期目录分配"的对等物。语言只是宿主；
**直接 `os/exec` 调用 bwrap 二进制，绝不引入 containers/go 容器生态**
（那会把方案拖向 podman 路线）。

### 2.3 为什么不用 C（GTK 的原生语言）

"GTK 是 C 库"这个前提成立，但它只约束**绑定层**，不约束**业务层**。判断依据是代码分布：本项目的绑定层调用都由 puregotk 生成的 GTK/libadwaita 绑定承担（`v4/adw` 178 个类型，按 GNOME SDK 重新生成），不是本仓库维护的代码；其余部分——探测管线、TOML 档案解析、`.desktop` 解析（`Exec` 的字段码/引号转义/`env VAR=x` 前缀）、`<id>` 校验、语义分类规则表、`BuildBwrapArgs`、实例 CRUD——与 UI 无关，占绝对多数。

按这部分来算 C 的成本：

- **输入全是不受信内容，且错误后果是安全性的**：`<id>` 直接拼进删除路径（`instances/<app>/<id>/`，路径穿越是最高危输入）；`/usr/share/applications` 的 397 个第三方 `.desktop`；社区分发的档案 TOML（§10 路线图第 7 项）；strace/overlay 的输出文本。C 里是手写 `strlen`/`snprintf` 边界与每条失败分支的 `free`，Go 里是边界检查 + `defer`。这里 bug 的形态不是"程序崩溃"，而是**隔离失效或删错目录**——静默发生、不可回滚。
- **单测是完成定义的一部分**：核心逻辑要求纯函数可单测（`AGENTS.md`）。Go 的 `testing` + 表驱动 + `t.TempDir()` 都在标准库；C 需要引入 cmocka/Unity 并接进构建系统、每个 case 另写 teardown。测试成本直接决定测试还会不会被写。
- **分发形态是 GitHub Releases 静态二进制**：C + glibc 的版本兼容（新系统编译的二进制在老系统缺 `GLIBC_2.xx` 符号）与静态链接 glibc 的 NSS/dlopen 问题，与"下载即用"直接冲突；Go 在 `CGO_ENABLED=0` 下交叉编译 amd64/arm64 无此问题。
- **进程编排密度高**：探测要起 fuse-overlayfs、起 bwrap、等用户操作完成、处理超时与中断清理（`trap`），GUI 侧探测还必须在后台执行（§4.7 线程模型）。C 里是 fork/exec + pipe + poll + 信号处理手写一遍，Go 里是 `os/exec` + `context` + goroutine。

**C 的收益上限要看清**：它省掉的只是绑定层这一层生成代码。若 puregotk 的 S0 闸门（§4.3）不过，退路是 gotk4（cgo 绑定），代价是 **GUI 二进制失去交叉编译能力**，core 仍是零 cgo 的 Go、仍可单测——即"绑定层有风险"的代价上限落在构建方式上，不落在整个 core 的语言上。

**改用 C 的成立条件**（当前都不成立）：core 收缩到几十行 syscall 封装；或产品要被合入 GNOME 上游组件、须遵循该组件的语言惯例。本项目是独立工具，且 core 是探测管线 + 解析 + 规则表 + CRUD。

## 3. 架构：单仓库，core 为心脏，CLI/GUI 为双视图

```
appclone/
├── core/               # 唯一核心：档案解析、实例 CRUD、bwrap 参数构造、discover 解析
├── cmd/appclone/       # CLI（cobra）——完整功能集，永久保留
├── cmd/appclone-gui/   # GUI（puregotk → GTK4/libadwaita）——core 的另一个视图
├── profiles/           # 内置档案模板库（随发行分发，只读；运行时档案在 ~/.local/share/appclone/profiles.d/）
└── docs/
```

**`profiles/` 的语义**：仓库内这份是**模板库**（首次生成时的预填来源、社区可贡献），
运行时真正生效的是 `~/.local/share/appclone/profiles.d/<app>.toml`（探测管线生成）。
两者不混用：程序**只写 `profiles.d/`**，内置模板只读；同名时运行时档案优先。

设计原则：

- **不搞"前后端分离"**：无 daemon、无 IPC 协议、无 REST。GUI 与 CLI 是同一
  core 包的两个薄视图，谁也不依赖谁；
- **CLI 永久保留**：核心用户可脚本化一切；GUI 崩溃不影响功能完整性；
- **core 为纯函数优先**：`BuildBwrapArgs(profile, instance) []string` 无副作用、
  可单测——这是整个项目测试价值的浓缩点。

## 4. GUI：GTK4 + libadwaita（puregotk 绑定）

### 4.1 不可让步的前提，与在此前提下的选型

产品定位是"像安卓设置里的一个开关"在桌面上的对等物——**桌面端的对等物就是 GNOME 设置页本身**。
因此框架边界只有一条，写死不让：**必须是 GTK4 + libadwaita 原生控件，不接受自绘 UI、不接受 webview**。
在此前提下才轮到"用哪个 Go 绑定"：

| 绑定 | cgo | 观感 | 成熟度 | 编译 |
|---|---|---|---|---|
| **puregotk**（选定） | **无**（purego/dlopen） | GTK4 + Adw 原生 | 自称 experimental，活跃（Codeberg，2026-10 仍在提交） | ~40s |
| gotk4（**兜底**） | 需要 gcc + dev 头文件 | GTK4 + Adw 原生 | 成熟，活跃（GitHub，2026-10-31 提交） | 慢（无缓存 10min+） |
| Fyne | 无 | **自绘，非原生** | 成熟 | 快 |

选 puregotk 的理由只有一句：**它是唯一同时满足"GNOME 原生"与"全仓零 cgo 单二进制"的选项**——
gotk4 牺牲后者，Fyne 牺牲前者。本项目 UI 只有三屏、控件面窄（行控件 + 开关 + 对话框），
恰好落在 purego 覆盖良好的区域；而 purego 的空白区（非指针 struct 参数、自定义 GObject 子类）本项目一个都不碰。

### 4.2 选 puregotk 的事实依据（2026-11-06 核实，非印象）

- 绑定面：`v4/adw` 包 **178 个类型文件**，本项目需要的控件逐一存在——
  `AdwSwitchRow`、`AdwNavigationView`、`AdwPreferencesPage/Group/Row`、`AdwToolbarView`、
  `AdwActionRow`、`AdwBanner`、`AdwAlertDialog`、`AdwStatusPage`、`AdwToastOverlay`
  （进度指示用 GTK 自带的 `GtkSpinner`，不依赖 1.6 才有的 `AdwSpinner`，理由见 §4.4）；
- 构建面：README 明示 `CGO_ENABLED=0` 可构建，无 C 工具链，交叉编译不受拖累；
- 维护面：仓库迁至 Codeberg 后仍在提交（最近 2026-10-02，"Gen: Re-run script"——仍在按 GNOME SDK 重新生成）；
- 本机实测前置已满足：GTK **4.24.1**、libadwaita **1.10.0**、`gcc`/`pkg-config` 均在。

### 4.3 风险与对策：S0 门槛先于一切 UI 代码

puregotk 自称 experimental，所以**不允许"边写产品边试"**。第一批 UI 代码只做一个 30 行的 S0 spike，
五条验收全过才继续；任一失败即整体回落 gotk4（`core` 不动，只换 UI 层，切换成本 = 三屏的重写）：

1. `AdwApplication` + `AdwNavigationView` 的 push/pop 正常；**Esc 只关最上层模态/探测页，不承担页面返回**（返回走 HeaderBar 后退按钮，与 §4.6 裁决一致）；
2. `AdwSwitchRow` 的 `notify::active` 能进 Go 回调（开关是本产品的核心交互，这条是生死线）；
3. `AdwAlertDialog` 异步响应能取到 response id（删除确认依赖它）；
4. 中文文本渲染正常 + 系统暗色切换自动生效；
5. **在 Ubuntu 24.04 环境（libadwaita 1.5.0 / GTK 4.14.2）内跑通三屏与删除确认**。门槛下调到 1.5 这件事，
   只有这一条过了才算成立——本机 1.10.0 上通过不能替代它：绑定里若出现 1.6+ 才有的符号，
   在 1.5 上同样是 `init` panic，与"UI 只用到 1.5 控件"无关。

其余对策：`go.mod` 锁定 puregotk 版本（**不追 main**，升级当独立任务并重跑 S0）；UI 只碰行控件与对话框。

**S0 结果（2026-11-07，`design/s0spike/` 可复跑，`CGO_ENABLED=0` 构建）**：五条全部通过，绑定层按原方案继续。
证据与边界：
- ① push/pop 与 `popped` 信号正常；**Esc 会触发 NavigationView pop**（Xvfb + xdotool 实测），
  与 §4.3"Esc 不承担页面返回"冲突——原生实现必须在窗口级拦截 Esc，不能依赖控件默认行为。
- ② `AdwSwitchRow` 的 `notify` 进 Go 回调并回读到 `active=true`（生死线通过）。
  附带约束：SwitchRow 只能放进 `GtkListBox`（`AdwPreferencesGroup` 提供），
  直接塞 `GtkBox` 会告警且行不可用。
- ③ `AdwAlertDialog` 的 `ConnectResponse` 拿到 handler id，response 信号以字符串参数分发到 Go（`"trash"`）；
  真实点击仍属 GUI 冒烟待办（S0 只验绑定能力）。
- ④ 中文标题渲染正常，`AdwStyleManager.GetDark()` 可读（本机会话为浅色）。
- ⑤ 在 `ubuntu:24.04`（GTK 4.14.5 / libadwaita 1.5.0）容器内 Xvfb 跑通同一程序、退出码 0，
  无 `init` 符号缺失 panic；**未覆盖三屏本身**——S0 验的是"绑定在 1.5 上可用"。
  三屏 v1 已落地（`cmd/appclone-gui/`，2026-11-08），在 1.5 上的控件子集未再加测；
  GTK 侧回归闸门已建立（`design/guitest-gtk.sh`：列表 → 确认 → 创建 → 详情 → 返回，
  以及首屏落点、空态映射、探测中取消/失败/可重试错误态、快速重推竞态、降级/致命横幅、en 文案九条用例），
  删除/重命名/关闭开关对话框与真实指针/键盘注入仍无自动化（见 `STATUS.md` 与 `NEXT.md` "被挡住的"）。

### 4.4 版本门槛与一条必须承认的边界

- **libadwaita ≥ 1.5**、**GTK ≥ 4.14**。门槛由两个约束的**交集**决定，不是只看控件清单：
  - **控件侧**（本机 `Adw-1.gir` 的 `version` 属性）：`AdwSwitchRow`/`AdwNavigationView`/`AdwToolbarView` = 1.4、
    `AdwBanner` = 1.3、`AdwEntryRow` = 1.2、**`AdwAlertDialog` = 1.5**（删除确认依赖它）。
    唯一要求 1.6 的是 `AdwSpinner`，**已弃用，进度指示改用 GTK 自带的 `GtkSpinner`**——这是为覆盖 Ubuntu 24.04 付的全部代价。
  - **发行版侧**（2026-11 实测，见 ADR-017）：Ubuntu 24.04 LTS = libadwaita **1.5.0** / GTK **4.14.2**；
    Ubuntu 26.04 LTS = 1.9.0 / 4.22.2；Debian 13 = 1.7.6；Fedora 41+ ≥ 1.6；Arch / openSUSE Tumbleweed = 1.10。
    取"仍在支持期的主流 LTS 中最低者"即 24.04 → **1.5 / 4.14**。
  - 原定的 1.6/4.16 是只看控件清单正推的结果，会把 Ubuntu 24.04 LTS（装机量最大的 LTS）排除在外；
    为一个 Spinner 控件放弃它，代价与收益不对称，故下调。
  - **明确不支持**：Ubuntu 22.04（libadwaita 1.1.7 / GTK 4.6）、Debian 12（1.2）、openSUSE Leap 15.6（1.4.2 / GTK 4.12）。
    前两个低于 1.4，连 `AdwSwitchRow` 都没有；Leap 15.6 有 SwitchRow 但整体低于 1.5 / 4.14 门槛。
    三者都不存在降级路径——不满足门槛就是 `init` panic，不是功能少一块。
- **门槛是硬约束，不是建议**：puregotk 在包 `init` 阶段 dlopen 并按符号名注册，库版本低于门槛是 panic 而非降级，
  所以发行版包必须把依赖写死（§6）。本机 1.10.0 / 4.24.1 满足。
- **边界（如实记录，不粉饰）**：puregotk 在包 `init` 阶段 dlopen 库，缺库时是 panic 而不是友好提示；
  Go 的 `recover` 拦不住 init panic，import 也无法条件化——所以**"缺 GTK 时给用户弹窗"这件事在单二进制里做不到**。
  保证手段改为：① 发行版包声明依赖并**写死版本下限**（`libgtk-4-1 (>= 4.14)` / `libadwaita-1-0 (>= 1.5)`，
     各格式的包名与写法见 §6——只写裸包名等于没写）；② Releases 二进制在 README 标注运行期依赖；
  ③ CLI 排障口 `appclone doctor`（含 GTK4/libadwaita 版本检查）——**面向开发者与支持场景**，
     不是让普通用户先去开终端（那违反用户零门槛红线；产品路径只有包依赖一条）。
- NixOS 等非标准路径：puregotk 支持 `PUREGOTK_LIB_FOLDER` / `PUREGOTK_<LIB>_PATH` 显式指定库路径，
  README 里写进"故障排查"一节，不写进主流程。

### 4.5 三屏的控件映射（GTK 原生，一版全上）

一版三个界面，全部用 libadwaita 原生控件，**不写自定义 CSS**：

| 屏 | 承载结构 | 细节 |
|---|---|---|
| ① 应用列表 | `AdwNavigationView` 根页 = `AdwToolbarView{ AdwHeaderBar, AdwViewStack }`，栈内两页（`on` / `all`）各一个 `AdwPreferencesPage` | 标题栏标题位放 `AdwViewSwitcher`（宽策略：图标 + 文字）：**两个平级视图「分身」/「所有应用」，各自独立滚动**——上下一体、两组同页的结构会让用户在同一条滚动里跨两个语义区（2026-11-08 按用户反馈改）。首屏落点按状态：已有实例 → `on`，一个都没有 → `all`（点亮开关是唯一能做的事），只决定首屏一次，后续 `renderList` 不弹走用户；创建成功后切回 `on`（返回时刚建出的实例就在眼前）。`AdwViewStackPage.SetBadgeNumber` 给「分身」页挂数量角标（libadwaita 1.10 实测**当前页也显示**，0 时隐藏；早先"只在非当前页显示"的判断是误读截图，已更正）。致命横幅（bwrap 缺失）下**整页只读**：开关、行点击、搜索全部停用（任何操作最终都要 bwrap，允许进详情只会让用户在一屏点不动的界面里迷路），但**视图切换仍可用**。搜索 `GtkSearchEntry` 只属于 `all` 视图（397 个应用必须有搜索，见 ADR-024）；`on` 视图分组「已开启分身」+ 底部 `AdwActionRow`「添加应用分身」（`list-add-symbolic`，切到 `all`）。两个视图用同一套 `AdwSwitchRow`（title=应用名，subtitle=`App.InstanceN` + `App.RunningN` 拼出「N 个分身」/「N 个分身 · M 个运行中」/「未开启 · 已有隔离档案」/「尚未生成隔离档案」/「档案已失效」，行首 `GtkImage` 应用图标 32px——`Icon` 主题名/绝对路径都认，取不到退 `application-x-executable`）；只有已开启行带行尾 `GtkButton` + `AdwButtonContent`（icon `go-next-symbolic`，`.flat`）进详情——未开启的应用没有实例可管，不进详情；libadwaita 没有 `AdwButton` 类型 |
| ② 应用详情 | `AdwPreferencesPage` | `AdwPreferencesGroup("分身实例")`，每行 `AdwActionRow`（实例名 + 运行状态）+ 尾部「启动」「删除」（**运行中两者都禁用**——`rm` 对运行中实例拒绝，退出码 7，界面不能更宽松）；`AdwPreferencesGroup("档案")` 只读行：隔离路径数、探测方式与置信度（**只说证据来源与覆盖范围，禁写"结果完整"**）、**共享但可写（`shared_writes`，含代价说明）**、单例处理、最近审计结论、实例根目录；重命名用 `AdwEntryRow`（GNOME 原生表单模式，不用行内 contenteditable） |
| ③ 探测页（**四态**） | `AdwPreferencesPage` + `AdwBanner` | **探测中**：`GtkSpinner` + 「正在跟踪「X」的写入…」+ **已发现 N 个写入条目**（流式；措辞不是"命中次数"）+ HeaderBar「取消探测」；**确认**：①「应用自身数据」可勾 `AdwSwitchRow`（含缓存项）②「单例处理」可勾（**由检出的单例项驱动，没检出就不出现**）③「公共目录（强制共享）」只读 ④**「共享但可写（不隔离）」只读且写明代价** + suggested-action「创建分身」（open 为「开启分身」、reconfig 为「创建分身」、rediscover 为「保存档案」）；**创建中**：「正在创建…」+ 确认与取消**都** disabled（第二次提交会建出两个实例，中途取消会留半成品）；**失败**：`AdwStatusPage` 按 `Reason` 分四套文案 + 「重试」（**恒可用**，不沿用上一态的 disabled）。降级时**同一 `AdwBanner` 从探测中起常驻、切到确认态不消失**，且列表页那条被「了解」收起后这里仍在（push 页盖住了窗口顶部横幅） |
| 反馈 | `AdwToastOverlay`（包住 NavigationView） | 创建/删除/重命名结果——**不用模态弹窗报成功**。`AdwToastOverlay` 一次只显示一条、新的顶掉旧的（不是队列），所以 toast 只承载"刚才那步成了"这种一次性确认，**关键结果必须同时体现在行内** |
| 空态 | `AdwStatusPage` **取代「分身」视图的列表** | **不取代另一个视图**：「所有应用」照常可达，空态里另有 suggested-action「选择应用」切过去——两者都断了才是首启死路（首启用户看不到任何开关）。**StatusPage 不能放进 `AdwPreferencesGroup`**：`AdwPreferencesPage.Add` 只收 Group，Group 又只收 `AdwPreferencesRow`，硬塞会被裁成半截图标（只剩一个灰色半圆）；实现直接挂在 `on` 视图的 `GtkBox` 里（`vexpand` 居中），`renderList` 只切 visible |
| 危险确认 | `AdwAlertDialog` | 两处：删除单个实例、关闭应用开关（会连带删除该应用全部实例）。**`default response` 必须是「取消」**——把 `destructive` 响应设成默认，回车即删，而对话框存在的意义正是"不要一键删"；纯提示型对话框（"还有分身正在运行"）才把「知道了」设为默认 |

### 4.6 与 `design/appclone-gui.html` 的裁决规则（冲突时以原生为准）

HTML 稿是**交互行为基线，不是视觉基线**。它不是 GTK 渲染的，照抄必然不原生。逐条裁决：

| 稿里这么做 | 原生实现改为 | 理由 |
|---|---|---|
| 点整行进详情 | 行尾 chevron 按钮进详情 | `AdwSwitchRow` 的 activate 已被开关占用，GNOME 惯例是加尾部按钮 |
| 顶部「深色主题」切换按钮 | **删除**，跟随系统（`AdwStyleManager`） | GNOME 应用不自带主题开关，这是稿子为了演示加的 |
| 自定义 CSS 配色/圆角/阴影 | libadwaita 内置样式类（`.dim-label`/`.monospace`/`.pill`） | 保持与系统设置页完全一致 |
| 屏3 做成窗口内遮罩对话框 | 改成 `AdwNavigationView` 的 **push 页** | 多步配置用导航页而不是模态（GNOME HIG），且对话框里套一长串勾选项很难滚动；探测中的"取消"也需要 HeaderBar 位置 |
| 自绘遮罩 + 对话框 | `AdwAlertDialog` | 仅用于**破坏性确认**（删除实例、关闭开关）；它自带正确的键盘与无障碍行为 |
| 底部 toast 条 | `AdwToast` | 同上 |

**稿子已补齐的交互缺口（2026-11-06，与本文同步修）**——这些不是"原生比稿子多"，而是稿子原本漏了、现在两边都有：

| 缺口 | 补法 | 为什么不能只在原生侧补 |
|---|---|---|
| 点「删除」立即执行（原 §4.6 唯一"原生多出"项） | 稿子加确认对话框，「移到回收站」为默认 | 稿子是交互基线；基线里没有确认，guitest 就无法防止"实现时忘了确认" |
| **关闭应用开关直接清空实例**（原实现一行 `instances.length = 0`，无确认无拦截） | 稿子加确认对话框，取消时开关回弹 | 关闭开关 = 删掉该应用全部实例数据（进回收站），破坏性与"删除单个实例"同级 |
| 列表页无搜索（397 个应用只能翻） | 稿子加 `GtkSearchEntry` 行为（按名称过滤） | 规模问题是产品问题，不是实现细节（ADR-024） |
| 屏3 只有"确认"一态（无探测中、无失败） | 稿子加 `probing` / `confirm` / `creating` / `error` 四态 | 探测十秒到一分钟，"点完开关一片空白"会被当成坏了 |
| 屏3 无「公共目录」只读组（公共目录混在候选里带锁标记） | 稿子改为独立只读组 | 方案设计 §6.1 第三条 UI 约束：用户必须看见"哪些被排除了" |
| 运行中实例可被删除（启动禁用了、删除没有） | 稿子改为两个按钮同时禁用 | 与 `rm` 的退出码 7 同源；界面比 CLI 宽松等于把契约撕开一个口子 |
| 「已开启分身」组按"有没有档案"分组（关掉开关后应用不回「所有应用」） | 稿子改为按"有没有实例"分组 | `supported` 只是"曾经配过"，`instances.length` 才是"已开启"；两者混用会让组名与行内状态自相矛盾 |
| **空态把整块列表藏掉**（`#listState` 整个 `display:none`） | 稿子改为：空态只取代「已开启分身」组，搜索与「所有应用」照常（2026-11-08 起落点改为「分身」视图，见下一行；不变式不变） | 引导语让用户"在下方「所有应用」里点亮开关"，列表藏掉了就指向一个不存在的区域——首启用户无路可走，这是产品首启唯一路径 |
| **「已开启」与「所有应用」同页上下叠**（一条滚动跨两个语义区；用户反馈里被点名） | 稿子与原生同改为：`AdwViewStack` 两个平级视图 + 标题栏 `AdwViewSwitcher`，各自滚动；空态只取代「分身」视图，另一个视图与切换器**始终可达**（空态里另有「选择应用」） | 两个列表语义不同（管理既有 / 挑选新增），同页会让用户在滚动中越过边界；但"空态可达性"这条不变式不能因拆视图而丢——首启仍必须有一条看得见的路去点亮第一个开关 |
| **确认到落地之间没有「创建中」态** | 稿子加 `creating` 第四态：按钮「正在创建…」、确认与取消都禁用 | 写档案 + 建目录 + 生成 `.desktop` 是几百毫秒事务，不是原子操作；连点两下建两个实例，中途取消留半成品 |
| 「共享但可写」（`shared_writes`）在界面上完全不可见 | 稿子加第四组只读组，**并写清代价** | 用户遇到"两个分身的最近文件混在一起"会判定产品坏了，而这是有意为之的取舍（§5.4） |
| 探测中计数写成「命中 N 次」 | 改为「观测到 N 个写入条目」 | overlay-diff 只能回答"写过/删过"，拿不到次数；写次数，实现期只能编 |
| 置信度写「结果完整（本次会话全部写入已捕获）」 | 改为「覆盖本次会话实际发生的写入（没走到的功能分支查不到）」 | 绝对承诺让用户不再复核，而漏隔的目录就此永久漏掉 |
| 入口只按"有没有实例 / 有没有档案"二分，**档案失效被当成可复用** | 拆 `has_profile` / `creatable`，加第五个入口「重新配置 · X」 | 复用失效档案 = 造一个什么都不隔离、启动即失败的分身（`--bind` 源不存在） |
| 破坏性对话框把「移到回收站」设为默认响应 | 默认响应改为「取消」；稿子给取消按钮加 `.default` 标记，回归断言回车不删 | 回车即删；对话框存在的意义正是"不要一键删" |
| Esc 语义未定义（无条件 `closeDialog`） | 三级语义：确认框 → 探测页 → **什么都不做**（不导航） | Esc 不是"返回"，返回靠 HeaderBar 后退按钮；`AdwNavigationView` 也不响应 Esc |
| 搜索逐键重建 400 行、命中不封顶 | 120ms 防抖 + 命中封顶 100 并提示"再输几个字" | 输入顿挫会被当成程序卡死；静默截断会被当成"这个应用不存在" |
| 副标题硬编码「1 个分身运行中」 | 用 `App.RunningN` 拼实际数量 | 两个分身同时开着却说 1 个，用户会以为另一个挂了 |
| toast 被当成唯一结果通道 | 明确 `AdwToastOverlay` 一次一条、新的顶掉旧的；关键结果必须同时落在行内 | 用户没读到的那两秒等于没告诉过他 |
| 未开启应用只有「配置…」按钮（与"开关是唯一主入口"冲突） | 稿子改为所有行统一 `AdwSwitchRow` | 一个产品不能有两套入口概念；Android 的对等物就是开关 |
| 创建门禁只数"任意勾选项"（只勾缓存也能建） | 稿子改为只数"应用自身数据" | 生成一个什么都不隔离的空档案 = 分身与主实例完全共享数据，用户却以为隔了 |
| 入口标题只区分 setup/clone，把"配过但关掉了"也写成「首次配置」 | 稿子拆成五入口五标题（setup / **reconfig** / open / clone / rediscover），open 跳过探测 | 标题说错要用户白等一分钟探测，还让人以为之前的档案白做了；且"档案失效"必须单独成入口（复用失效档案 = 造一个启动即失败的分身） |
| 致命横幅一旦打开，整页交互（含状态演示）再也回不来 | 稿子改为状态演示控制条不被 `blocked` 短路 | 状态机必须双向可达：只能进不能出的状态与"界面卡死"是同一件事，产品侧对应"环境恢复了但界面卡在不可用" |
| 失败态「重试」沿用上一轮 confirm 算出的 disabled | 稿子改为进 error 态时重算门禁（恒可用） | 失败态只有「重试」「取消」两条出路，点不动的重试等于让用户关窗口 |
| open 入口取消后开关停在"开"（零实例） | 稿子改为 `closeDialog` 一律按模型重渲染列表 | 取消 = 什么都没发生；留一个开着但零实例的假开关，比回弹更糟 |

**`design/guitest.js` 的定位要收窄**：它加载的是 `design/appclone-gui.html`，验的是**稿子**的交互可达性与状态变化
（不验像素）。它守的是 HTML 交互基线，**不覆盖 GTK 实现**——原生实现后另建 GTK 侧冒烟（S0 spike 的断言 + 后续
`broadway` 或 `xdotool` 驱动的三屏冒烟），两者不互相替代。同理，稿里那些将被原生删除的交互（主题切换按钮、
整行点击）在 guitest 里继续存在，直到 GUI 二进制取代 HTML 稿成为基线时才一并改写断言——现在删掉它们，
等于让 HTML 稿失去回归保护。

### 4.7 线程模型：探测绝不卡主线程

- 探测/创建/删除都是秒级到十秒级任务，**一律进 goroutine**，用 `context.Context` 支持取消；
- GTK 非线程安全，UI 更新一律 `glib.IdleAdd` 回主线程（`core` 只给 `ProgressFn` 回调，不认识 GTK）；
- 进度用 `AdwBanner` + 行内 `GtkSpinner` 呈现，**不用模态对话框堵住窗口**（用户要能中途取消）；
- `core.Probe(ctx, app, ProgressFn)` 是唯一入口，GUI 侧 `ProgressFn` 内部转 `IdleAdd`——这层转换是 GUI 包唯一的 GTK 耦合点。

`ProgressFn` 必须能表达"还在动"，否则十秒的静默等于卡死。事件结构（`core.ProbeProgress`，`core` 定义，GUI 只渲染）：

| 字段 | 用途 |
|---|---|
| `Stage` | `round_done`（一轮会话结束，附合并后的条目数）/ `fallback`（无 fuse-overlayfs，本次改走 strace） |
| `Round` / `Rounds` | 第几轮 / 共几轮；两轮是默认值（登录态往往第二轮才落盘） |
| `Entries` | 已发现的**写入条目数**（不是路径数、不是命中次数，§6.1 文案红线）；每轮结束流式递增，数字在涨就说明没卡 |

实现口径（2026-11-08）：`fallback` 事件在**目标应用未运行的闸门之后**才发——闸门失败时用户该看到的是
`app_running`，不是一条不会发生的 fuse 降级横幅。`checking`/`collecting` 这类闸门级事件不进
`ProbeProgress`：GUI 自己就有 `probing` 态，多一个 core 侧枚举只会两处不同步。

**取消必须真的传播**：`ctx.Done()` 要一路传到 `kill(-pgid)` 与 `fusermount3 -u`，
不能只把 UI 切回列表就完事——留下的进程会继续写 upper，下次探测拿到一份混了两个会话的结果。
取消后的状态是"什么都没发生"：档案不写、实例不建、`probe/` 清空。

**回调槽纪律（purego 的硬上限，2026-11-08 复审发现并修复）**：purego 给每个 `*func` 变量地址在进程内
**永久**占一个 C 回调槽，上限 **2000**（`purego/syscall_sysv.go` 的 `maxCB`；`NewCallbackFnPtr` 只按变量地址复用）。
所以**渲染路径里禁止"每次新建闭包变量再 Connect"**——列表每重建一次（搜索每敲一个字都会重建）就要
几十个槽，一小时内必然 `panic: purego: the maximum number of callbacks has been reached`。
GUI 的做法（`cmd/appclone-gui/handlers.go`）：C 槽只由 5 个 `callbacks` 字段独占（notify / clicked /
activated / response / idle，地址恒定、注册一次后不再重新赋值），渲染只往按 GObject 指针索引的
**处理器表**里登记纯 Go 闭包；页面重建时整表注销（`renderScope.close()`）。新增控件一律走
`renderScope` 的三个登记方法（登记即连接），不要回到 per-widget 闭包的写法。

**四态是一个状态机，不是四段 if**：`probing → confirm → creating → done`，`probing → error`；
`probing` 期间「取消」可用、「创建」不存在（不是禁用）；`creating` 期间**取消也不可用**（事务已经开始）；
`error` 只允许「重试」与「取消」。
把这三/四个状态写成互斥枚举，别用"按钮禁用与否"隐式表达状态——那正是稿子原来只有一态时的问题来源。

**运行态轮询（不能靠 pgrep，也不能一直全量扫）**：

| 项 | 策略 | 理由 |
|---|---|---|
| 详情页实例运行态 | 1s 轮询该应用的实例 `run.lock`（`LOCK_NB` 试探） | 用户在应用里退出分身，界面要在一秒内反映出来，否则「删除」会一直是灰的 |
| 列表页「M 个运行中」 | 每两拍（2s）轮询**只有实例的应用**（`InstanceN > 0`） | 全量 397 个应用 × flock 是纯浪费；无实例的应用运行数恒为 0，不必问 |
| 窗口不可见 | **暂停轮询**（`GtkWindow::GetVisible`） | 最小化/切到别的桌面还每秒开文件是耗电，也不礼貌 |
| 轮询失败（IO 错误） | 不弹错、不动界面（保持上一次的数字） | 运行态查询失败不该打断用户操作；下一次轮询自己会恢复 |

### 4.8 应用标识与桌面集成（GNOME 原生行为都是白拿的）

- app id：`io.github.mensylisir.Appclone`（决定 WM class、通知归属、DBus 名）；
- `GtkApplication` **自带单实例 + 二次启动时 present 已有窗口**——不要自己写锁文件（这与应用分身的单例问题无关，别混）；
- 桌面入口文件名**必须是 `io.github.mensylisir.Appclone.desktop`**（与 app id 同名）：GNOME 靠
  "desktop 文件名 == app id"把运行中的窗口归属到入口，名字不一致会出现 Dock 重复图标、通知不归属本应用，
  且事后改 `Icon=` 补不回来。字段为 `Name=应用分身`、`Categories=Settings;System;`、`Icon=` 同 app id、`StartupNotify=true`；
- **图标不做角标合成**：GNOME 没有 Overlay Icon 机制，分身靠 `Name` 后缀「（分身 <display_name>）」区分（显示名，非目录 `id`）；
  带角标图标列为可选后续，不进 v1；
- **`Name` 有长度上限（40 rune，含后缀）**：GNOME 概览标签在小图标下约 12 个全角字符后截断，
  超长会让两个分身看起来一模一样（被切掉的正好是后缀）。超限时截断 `display_name` 部分补 `…`；
  `display_name` 本身在 `core` 限 32 rune（与 `id` 对齐），两处上限各有理由，别只留一个；
- **本地化只翻模板**：`Name[zh_CN]`/`Name[en]` 由 `core/i18n` 拼出（ADR-025），`display_name` 原样嵌入不翻译；
  无 `[lang]` 的 `Name=` 作为兜底必须写。

### 4.9 被否决的备选

| 备选 | 否决原因 |
|---|---|
| Wails / Electron（webview） | UI 仅三屏，浏览器壳纯属浪费——本项目拒绝"桌面前后端分离"；且非原生 |
| Fyne | 纯 Go 零依赖是优点，但自绘 UI 在系统设置类工具上没有原生观感——**与"保持 GNOME 原生"直接冲突**。**不设"无 GTK 环境兜底"**：那要另写一套三屏，兜底版本必然与主版本行为分叉，且目标用户（GNOME 桌面）本来就有 GTK |
| Qt bindings（therecipe/qt 等） | Go 侧生态不成熟，且 Qt 观感在 GNOME 桌面上不原生 |
| gotk4（本版不用，留作兜底） | 观感与 puregotk 等价，但需 cgo：构建机要装开发包、编译慢一个数量级、交叉编译退化。**只在 S0 门槛不通过时启用** |

~~一版只做列表 + 开关；discover 勾选界面放二版。~~ **已修订（ADR-006）**：一版三个界面全上。
勾选屏不是增强项——缺了它，自动生成的档案无人复核就直接生效。

## 5. 数据格式与版本策略

- **档案格式**：TOML，运行时落 `~/.local/share/appclone/profiles.d/<app>.toml`，含 `version` schema 字段；
  仓库内 `profiles/` 只是只读模板库（§3 语义说明）。
  早期 bash 原型的 `.sh` 档案无存量用户，不做迁移器，直接以 TOML 起步；
- **`version` 的三种读法**（细则见[方案设计.md](方案设计.md) §5.5）：等于当前版本直接解析；
  小于当前版本走显式迁移函数链；**大于当前版本拒绝加载并报错**（不猜新字段语义）。
  未知字段原样保留（round-trip），否则旧版改个显示名就会把新版字段抹掉；
- **所有状态文件原子写**（`tmp + fsync + rename`），写操作持 `AC_BASE/.lock`（ADR-022）——
  TOML 不是数据库，但没有原子替换就会出现"档案半截、解析失败、用户以为坏了"；
- **实例数据**：目录布局沿用方案文档 §4（`instances/<app>/<id>/root/<原路径>`），
  CLI 与 GUI 共用同一布局——**双视图互操作优先于格式洁癖**；
- **删除语义**：一律回收站（gio trash），永久删除仅在回收站内提供。
- **GUI 自身偏好**（窗口尺寸、上次选中的应用）写 `~/.local/share/appclone/ui.toml`，
  **不引入 GSettings schema**（ADR-014）：单二进制分发无法把 `.gschema.xml` 装进
  `/usr/share/glib-2.0/schemas` 再跑 `glib-compile-schemas`，为一个窗口尺寸不值得。
  `ui.toml` 损坏时静默重建默认值——它不是用户数据，不需要错误处理。
- **应用元数据缓存**（`cache/apps.toml`）：397 个 `.desktop` 的解析结果 + 每个源文件的 `mtime+size`。
  它**不是**用户数据，可随时删；缓存未命中时重扫，不报错（ADR-024）。

## 6. 构建与分发

- **构建**：全仓 `CGO_ENABLED=0`——core/CLI 本来如此，GUI 因 puregotk 走 purego/dlopen 也成立（ADR-005 二次修订）。
  因此 `GOOS=linux GOARCH=arm64 go build ./cmd/appclone-gui` 在 amd64 机器上就能出二进制，
  构建机不需要 GTK 开发包（**这是换 puregotk 换来的最大实惠**）；
- **操作系统边界**（"平台"这个词在本项目专指 OS，别和 CPU 架构混）：**只 Linux**。这不是"暂时没做
  Windows/macOS"，而是原理不适用——整套机制建立在 Linux 的 mount namespace + 非特权 user namespace 上
  （bwrap 就是 Flatpak 的沙箱引擎，只存在于 Linux）。macOS 的等价物是 App Sandbox、Windows 是
  AppContainer/MSIX，目录语义与启动方式都不同，那是另一个产品，不是待办项。
- **CPU 架构：amd64 + arm64，arm64 是一等公民**（不是"顺带兼容"）：
  - **可行性已核实**：puregotk 依赖的 purego 把 `Linux: amd64, arm64` 列在 **Tier 1**（README 明示，
    Tier 1 的严重 bug 会阻塞发版），所以"零 cgo 出 arm64 GUI"成立，不是纸面推测；
  - **构建方式**：`GOARCH=arm64 go build` 在 amd64 机器上直接出二进制，构建机不需要 arm 机器，
    也不需要 GTK 开发包（这是 puregotk 的实惠，gotk4 做不到）；
  - **两个架构的差异只在打包，不在运行时**：bwrap / fuse-overlayfs / mount namespace 都是内核侧，
    amd64 与 arm64 行为一致；真正的差异是三套包格式的**架构命名**（见下"架构命名"）；
  - **"能编译"≠"验证过"**：arm64 必须在真实 arm64 环境跑一遍 GUI 冒烟才算支持。CI 用 GitHub 的
    `ubuntu-24.04-arm` hosted runner 加一个 job（该标签已 GA），不要只靠交叉编译就宣布 arm64 可用。
    这条属于**发版闸门**，不进 S0 五条——S0 验的是"puregotk 在 1.5 上能不能用"，架构与它无关，混进去
    会让 S0 从半天膨胀成两个环境。
- **运行期依赖**：GUI 需要 GTK ≥ 4.14 + libadwaita ≥ 1.5 的**运行库**（不是开发包；门槛推导见 §4.4）。
  它**不是动态链接**：puregotk 在 `init` 阶段 `dlopen`，二进制没有 `DT_NEEDED`。所以二进制本身是静态的，
  但运行期仍要经 dlopen 解析 libc 与 GTK 符号——**宿主必须是 glibc 系统**（musl/Alpine 不在支持范围）。
  因此 `.tar.gz` 的支持下限是"宿主提供的 libadwaita ≥ 1.5"，不是二进制自身的 glibc 版本。
- **阻断级前提：Ubuntu 24.04+ 的 AppArmor 限制**（不解决这条，装上也起不来）：
  Ubuntu 23.10 起默认 `kernel.apparmor_restrict_unprivileged_userns=1`，未受限的 `bwrap` 创建 user namespace
  会被拒，报 `bwrap: setting up uid map: Permission denied`。**Ubuntu 的 bubblewrap 包不带 AppArmor profile**
  （Launchpad #2144531，2026-03 仍为 New），包里的 `50-bubblewrap.conf` 只设了旧的
  `kernel.unprivileged_userns_clone`，对这个限制无效。
  处理办法（用户零参与，由 deb/rpm 的 postinstall 完成）：检测该 sysctl 为 1 时安装
  `/etc/apparmor.d/bwrap`（`profile bwrap /usr/bin/bwrap flags=(unconfined) { userns, }`）并 `apparmor_parser -r`；
  **目标文件已存在则跳过**（发行版将来自己提供时让位）。
  不采用"把 sysctl 全局置 0"的方案——那会削弱整机安全加固，影响面远大于本工具。
  `appclone doctor` 同步检查这一项（命令表见 [方案设计.md](方案设计.md) §6）。
- **分发产物**（v1.0.0；**deb 与 rpm 各出 amd64 + arm64 两套，Arch 只出 x86_64**，见下）：
  三种包格式由 CI 用 **nfpm** 从同一份 `nfpm.yaml` 产出；`.tar.gz` 不走 nfpm——nfpm 没有 tarball packager，
  由 CI 另起一步按同一份 `contents` 清单打 tar。
  - `.deb` → Ubuntu 24.04+ / Debian 13+；`Depends: libgtk-4-1 (>= 4.14), libadwaita-1-0 (>= 1.5), bubblewrap`，
    `Recommends: fuse-overlayfs`；
  - `.rpm` → Fedora 41+ / CentOS Stream 10 / RHEL 10；
    `Requires: gtk4 >= 4.14, libadwaita >= 1.5, bubblewrap`，`Recommends: fuse-overlayfs`；
    **openSUSE 不在 v1 承诺内**：Tumbleweed 的 GTK4/libadwaita 运行库包名与 Fedora 不同，单份 rpm 的 `Requires`
    只能命中一套，要支持得另建 spec——留待社区走 OBS，不写进承诺。
  - `.pkg.tar.zst` → Arch / Manjaro（**仅 x86_64**）；`depends=(gtk4>=4.14 libadwaita>=1.5 bubblewrap)`，
    `optdepends=(fuse-overlayfs)`；同时提交 AUR `appclone-bin`（预编译，只含 x86_64）。
    **Arch 的 arm64 走另一条路**：Arch Linux ARM 是独立项目、官方仓库只发 x86_64，所以 arm64 用户用 AUR 的
    源码包 `appclone`（本地编译，零 cgo 不需要装 GTK 开发包）或 `.tar.gz`——这条要写进 README，别让 arm64 用户去装
    `appclone-bin` 然后拿到一个跑不起来的 x86_64 二进制；
  - `.tar.gz`（裸二进制 + `.desktop` + 图标，同上清单）→ 兜底，覆盖其他 glibc 发行版，README 写清运行期依赖。
    **命名必须带架构**：`appclone_<version>_linux_<goarch>.tar.gz`（如 `appclone_1.0.0_linux_arm64.tar.gz`）——
    两个架构同名会在 Releases 里撞车，arm64 用户也无从辨认；
  - 每个 release 附 `checksums.txt`；GPG 签名在 v1 可选。

  **版本约束不是可选的**：`libadwaita-1-0` 这个包名在 Debian 12（1.2）和 Leap 15.6（1.4.2）上同样满足，
  只写裸包名会让包管理器"装成功"，随后 GUI 在 `init` 阶段 panic——这是最难查的故障形态。写死下限，不写上限。

  **架构命名**（`GOARCH` 名与各格式的架构串不同，写错的表现是"包做出来了但装不上这个架构"）：
  `nfpm.yaml` 里 `arch: ${GOARCH}`（用 Go 命名），nfpm 翻译成各格式的架构串——deb 是 `arm64`、
  rpm 是 `aarch64`、Arch 是 `aarch64`；翻译不合预期时用 `{format}.arch` 单独覆写。
  **CI 矩阵不是 2 × 3**：`{deb, rpm} × {amd64, arm64}` 出 4 套，`archlinux` 只跑 amd64 出 1 套，
  合计 **5 个包** + 2 个 `.tar.gz`。`archlinux × arm64` 必须从矩阵里排除——产出它等于自己推翻
  上面"Arch 仅 x86_64、arm64 走源码包"的结论。

  **三种格式的成本不是三倍**：nfpm 一份配置，包名差异由 `overrides.{deb,rpm,archlinux}.depends` 承担
  （GTK4 运行库在 Debian 系叫 `libgtk-4-1`、在 Fedora/Arch 叫 `gtk4`）。
  `fuse-overlayfs` 一律走推荐/可选：缺它只影响探测质量（降级 strace），不影响启动。

  **包内落点**（`nfpm.yaml` 的 `contents`；漏一项的后果写在括号里）：
  - CLI → `/usr/bin/appclone`
  - GUI → `/usr/bin/appclone-gui`
  - 桌面入口 → `/usr/share/applications/io.github.mensylisir.Appclone.desktop`
    （**文件名必须等于 app id**，见 §4.8；不一致会导致 Dock 出现重复图标、通知不归属本应用）
  - 图标 → `/usr/share/icons/hicolor/scalable/apps/io.github.mensylisir.Appclone.svg`

  `postinstall` 必须做三件事：① AppArmor 检测与 profile 安装（上面那条）；② `update-desktop-database -q`；
  ③ `gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true`。
  deb 没有 pacman 那样的自动触发器，漏了 ②③ 的表现是"应用列表里搜不到、图标空白"。
- **v1 不做**（各有理由，不是遗漏）：
  - **Flatpak**：见 ADR-015；
  - **Snap**：同类问题（confinement 阻止调 bwrap），另需商店账号与审核流程；
  - **AppImage**：依赖 libfuse2（多数新发行版默认不装），且无法把 `.desktop` 装进系统目录——
    与"分身图标出现在应用列表"的产品形态直接冲突；
  - **COPR / PPA / 进发行版官方仓库**：需要账号与长期维护责任。原文档写的"外包给社区"是空头承诺，
    社区不会凭空出现维护者——v1 由 CI 自己出包，不挂这条链路；用户量起来后再谈。
- **CI**：GitHub Actions——PR 触发 `gofmt`/`go vet`/`staticcheck`/`go test`，以及两个二进制 × 两个架构的交叉编译
  （无需装 GTK）；**tag 触发 nfpm 出包**并上传 Releases（矩阵同「架构命名」段：`{deb,rpm} × {amd64,arm64}`
  共 4 套 + `archlinux × amd64` 1 套 = 5 个包，`.tar.gz` 另出 amd64/arm64 两个）。
  GUI 冒烟要装 GTK4 运行库（`xvfb-run` + `broadway` 可选），跑**两个 job**：
  - `ubuntu-24.04` → 验 S0 第 5 条（libadwaita 1.5.0 下限，§4.3）；
  - `ubuntu-24.04-arm` → 验 arm64（**交叉编译出来能跑 ≠ 跑过**，见上方 CPU 架构段）。
  两个 job 的断言相同：能起、三屏能切换、**删除实例弹出 `AdwAlertDialog` 确认且可取消**（§4.6 要求补的那条），
  不做像素比对。
- **许可证**：建议 GPL-3.0-or-later（与 GTK/libadwaita 生态一致性最好；puregotk 为 MIT，兼容；
  core 为 MIT 亦可接受，若希望被第三方嵌入）。

## 7. 决策记录（ADR 索引）

| # | 决策 | 状态 |
|---|---|---|
| 001 | 隔离引擎选 bubblewrap，直接 exec 其二进制，不引入容器库 | 已定 |
| 002 | ~~v1 bash 长期保留为 fallback~~ → **已废弃**：产品直达 Go，bash 原型归档 `legacy/` 仅供开发对照（2026-11-05，理由：目标用户不接受 CLI 操作，bash 版不构成可用产品） | 已改 |
| 003 | 实现语言选 Go（原"v2 迁移"改为唯一实现路径） | 已改 |
| 004 | 桌面应用不做前后端分离：CLI/GUI 共享 core，无 daemon | 已定 |
| 005 | GUI 必须 GTK4 + libadwaita 原生控件（自绘/webview 一律否决）。**2026-11-06 二次修订**：绑定由 gotk4 改为 **puregotk**（GTK4+Adw 原生、purego 无 cgo、`CGO_ENABLED=0` 可构建）；gotk4 降为兜底，切换闸门 = S0 五条验收（SwitchRow 信号 / NavigationView push-pop / AlertDialog 响应 / 中文与暗色 / Ubuntu 24.04 实机） | 已改 |
| 006 | ~~GUI 一版范围锁定"列表 + 开关"，discover 勾选放二版~~ → **2026-11-06 修订**：一版即含三屏（列表 / 详情 / discover 勾选）。理由：没有勾选屏，自动档案直接生效，误判就会隔离公共目录 | 已改 |
| 007 | 档案格式 TOML（无 .sh 迁移负担）；实例目录布局 CLI/GUI 一致；仓库 `profiles/` 为只读模板库，运行时档案在 `profiles.d/` | 已改 |
| 008 | 路径探测：overlay-diff 为主探测、strace 降级兜底、静态预填 + 语义分类三层管线（详见 [discover.md](discover.md)） | 已定 |
| 009 | 应用来源同时扫 `/usr/share/applications`、`~/.local/share/applications`、Flatpak 导出目录，用户级覆盖系统级（只扫用户级会漏 97% 应用） | 已定 |
| 010 | 删除一律 `gio trash`（实例目录 + 桌面入口），**不设私有 `.trash/`**；永久删除只由用户在回收站完成 | 已定 |
| 011 | 实例 `id` 硬约束 `[a-z0-9][a-z0-9_-]{0,31}`，**省略 id 自动取第一个空闲（main/inst2…）、显式 id 已存在才报错**（原"缺省 main 一律报错"作废）；`id` 创建后不可变，重命名只改 `instance.toml` 里的 `display_name`；校验在 `core` 单一实现 | 已改 |
| 012 | 单例处理三分：普通文件锁用 bind 空文件；unix socket 用 `isolate_runtime`；DBus 用 `isolate_dbus`。**socket 禁止塞进 `lock_files`** | 已定 |
| 013 | GUI 三屏全部用 libadwaita 原生控件（SwitchRow/ActionRow/AlertDialog/Toast/StatusPage）；`design/appclone-gui.html` 是交互基线**不是视觉基线**，冲突时以原生为准（裁决表见 §4.6） | 已定 |
| 014 | 不引入 GSettings schema：GUI 偏好写 `~/.local/share/appclone/ui.toml`（单二进制分发无法安装 schema 并编译） | 已定 |
| 015 | v1 **不做 Flatpak 分发**：GUI 需宿主全可见 + 在宿主 namespace 起 bwrap，与沙箱模型冲突；走发行版包 + Releases | 已定 |
| 016 | **操作系统 = 只 Linux；CPU 架构 = amd64 + arm64 一等公民**（bwrap 只存在于 Linux，macOS/Windows 属另一套机制，不是待办；arm64 的可行性由 purego Tier 1 覆盖 linux/arm64 保证，但必须用 `ubuntu-24.04-arm` runner 实跑冒烟才算验证）。分发产物四件：`.deb`（Ubuntu 24.04+/Debian 13+）、`.rpm`（Fedora 41+ / CentOS Stream 10 / RHEL 10；openSUSE 需另建 spec，v1 不承诺）、`.pkg.tar.zst` + AUR `appclone-bin`（Arch）、裸 `.tar.gz` 兜底；三种包格式由 **nfpm 一份配置**产出（`.tar.gz` 由同源清单另打 tar），依赖**写死版本下限**。**Ubuntu 24.04+ 的 AppArmor userns 限制必须由 postinstall 安装 `/etc/apparmor.d/bwrap` 解决**（否则 bwrap 建 userns 被拒，见 §6）。Flatpak 见 ADR-015；Snap / AppImage / COPR / PPA 不做 | 已定 |
| 017 | **GUI 版本门槛下调为 libadwaita ≥ 1.5 / GTK ≥ 4.14**（原 1.6/4.16 是按控件清单正推，会排除 Ubuntu 24.04 LTS = 1.5.0 / 4.14.2）。代价：弃用 1.6 才有的 `AdwSpinner`，进度改 `GtkSpinner`。生效前提 = S0 第 5 条（Ubuntu 24.04 环境实跑通过）；该条不过则本结论作废，回到 1.6/4.16 | 已定 |
| 018 | **探测期的隔离契约**（三条，缺一条就出事）：① XDG 公共目录、`AC_BASE`、`shared_writes` 必须在 overlay 之上 bind 回**真实**目录——否则用户在探测会话里存的文件落在 upper，探测结束删 upper 等于删用户文件；② 探测期**不看档案、强制隔离单例**（私有 runtime + 私有 DBus + L0 空锁文件），否则主实例在跑时应用转发参数后退出，探测零写入；③ 探测前扫进程挡住"应用还开着"，不给"仍然继续" | 已定 |
| 019 | **`doctor --verify` 只在有运行期审计层时下结论**：`run --audit` 复用探测的 overlay 原语（lower=`$HOME`、upper=`/var/tmp/appclone/audit.<uid>/<app>.<id>/upper`），对账 `paths`/`shared_writes` 得 `verified`/`leak-detected`；没有审计层输出 `unverified`（不是"通过"）。免 root 下 fanotify 需 `CAP_SYS_ADMIN`、eBPF 被 `unprivileged_bpf_disabled` 关、inotify 无 PID——**不做假装能归因的检查**。审计默认关闭（全 `$HOME` 过 overlay 有 copy-up 开销） | 已定 |
| 020 | **进程生命周期归 `appclone run`，不归 bwrap**：`--die-with-parent` 只保证直接子进程，应用自己 `fork`+`setsid` 的进程逃得掉（bwrap 0.13.1 实测）。`run` 不 exec，`setsid` 起 bwrap 并常驻为父进程，退出/收到信号时对整组 `SIGTERM`→3s→`SIGKILL`；探测期共用同一实现。`--die-with-parent` 保留为兜底 | 已定 |
| 021 | **参数集去掉 `--unshare-uts`**：无隔离收益（hostname 副本本就与宿主一致），却让 namespace 内 hostname 与共享的 `/etc/hostname` 存在不一致可能，Java `getLocalHost`/hostname 自检类应用会走异常分支。`--unshare-pid`/`--unshare-ipc` 保留（有归因与 `--proc` 语义） | 已定 |
| 022 | **状态一致性按多进程处理**（桌面入口、GUI、CLI 会同时动作）：写操作持 `AC_BASE/.lock`（`flock(LOCK_EX)`，等 5 秒超时，退出码 6）；所有 TOML 写入 `tmp + fsync + rename`；实例创建用 `os.Mkdir` 的 `EEXIST` 作唯一判重（**不在锁外 stat+Mkdir**，那是 TOCTOU） | 已定 |
| 023 | **运行状态判定用 `run.lock` 的 flock**，不用 `pgrep`（同名主实例让判定永真）、不用 PID 文件（崩溃后 stale 假阳性）：`run` 持锁 → GUI 用 `LOCK_NB` 试探，"拿得到=没跑、拿不到=在跑"，进程死锁由内核释放 | 已定 |
| 024 | **397 个应用的列表策略**：`GtkSearchEntry`（名称 + desktop id 双字段过滤）+ 默认只渲染前 30 + 组尾「显示全部 N 个应用」；元数据并发解析并缓存到 `cache/apps.toml`（带源文件 `mtime+size` 校验）。**不做 `GtkListView` 虚拟化**——`AdwPreferencesGroup` 不是虚拟化列表，改造成本要用整个 boxed-list 观感去换，数量级到四位数再谈 | 已定 |
| 025 | **i18n 走 `core/i18n` 的 Go 消息表**（zh_CN + en，`LANG` 前缀匹配，其他语言回落英文），**不用 gettext**（与零 cgo 单二进制一致，且 `.mo` 文件还要装到 `/usr/share/locale`，与"单二进制 + 包依赖"的分发形态不匹配）。界面字符串不得硬编码在 GUI 代码（**落地节奏见 ADR-027**）；用户数据（实例显示名、应用名）不翻译；`.desktop` 的 `Name[zh_CN]`/`Name[en]` 同源 | 已定 |
| 026 | **私有 runtime / 私有 DBus 的生命周期**：`runtime/` 随实例复用（启动前清残留 socket）；`dbus-daemon --session --nofork --print-address` 由 `run` 起子进程（`--nofork` 必须带，否则父进程先退拿不到地址），与 bwrap 同生共死，结束后杀 daemon 删 socket；缺 `dbus-daemon` 时**降级为共享并明确告知**（"可能无法双开"），不静默 | 已定 |
| 027 | **GUI 文案本地化分期**（ADR-025 的落地节奏）：v1 先以 zh_CN 单语交付、`u.lang` 固定 `zh_CN`，退出条件 = GTK 闸门**走遍三屏四态**后三屏文案按 `ui.*` 入表、补 en 译文，`u.lang` 改回 `i18n.Lang()`。**已于 2026-11-08 退场**：闸门新增 `TestGtkSetupStates`（probing 取消 / 降级横幅「了解」）、`TestGtkErrorState`（error 无重试）、`TestGtkRetryableError`（重试接线）、`TestGtkRapidRepush`（导航页唯一 tag，守着 pop 过渡中再次 push 的竞态）与 `TestGtkFatalBanner`（致命态横幅、搜索、开关联动），加上生命周期用例的 confirm→creating 与详情页（共 9 条），列表/配置/详情 × probing/confirm/creating/error 全覆盖；三屏约 150 条 `ui.*` 文案（160 处 `tr()` 调用）全部进 `core/i18n`（zh_CN + en），`newUI` 统一用 `i18n.Lang()`。保障手段：`core/i18n/i18n_test.go` 钉住两语言键一一对应、占位符一致、en 表无中文、切片参数分隔符随语言，并扫描 `core/*.go` 的字面量 `err.*` 键（落地时抓出 `err.profile.exec_field_code` / `err.profile.extra_bind_missing` 只有 en、中文界面会回落英文的真实缺陷，以及三个 core 缺失键），`TestGtkEnglishStrings` 在 `LC_ALL=en_US.UTF-8` 下断言 en 界面文案。**仍禁止中英混排**：动态文案（reason、core 错误）与静态文案继续共用同一张表 | 已定（2026-11-08 退场完成） |
