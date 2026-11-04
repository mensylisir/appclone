# appclone 技术选型

> 本文记录 appclone 项目从原型（bash）演进到产品（Go + GUI）的完整技术选型决策，
> 以及每项决策的理由与被否决的备选方案。与《Linux应用分身完整方案.md》配套阅读。

## 0. 选型总览

| 层 | 选型 | 定位 |
|---|---|---|
| 隔离引擎 | bubblewrap (bwrap) | mount namespace 沙箱，唯一核心依赖 |
| 路径发现 | strace (`-e trace=%file`) | 无侵入摸清应用写入路径 |
| 克隆起步 | fuse-overlayfs | 无 root 的 copy-on-write，分身继承主实例数据 |
| 桌面集成 | .desktop 标准 + gio trash | 图标入口 / 回收站删除 |
| 核心层（Go） | Go ≥ 1.22，package `core` | 档案解析、实例管理、bwrap 参数构造 |
| CLI | cobra | `cmd/appclone`，永久保留 |
| GUI | gotk4 + libadwaita | `cmd/appclone-gui`，core 之上的薄 UI |
| 档案格式 | TOML（v2 起从 `.sh` 迁移） | 结构化 per-app 路径知识库 |
| 构建/分发 | Go 单二进制 + 各发行版包 | 无编译期 cgo，交叉编译友好 |
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

## 2. 实现语言：bash（v1）→ Go（v2）

### 2.1 v1 bash 的定位

原型期正确选择：工具本质是"参数构造器 + exec"，bash 完全胜任，零额外依赖
（装完 bwrap 即可用），对开源项目冷启动至关重要。**v1 长期保留为
最小依赖 fallback**，与 v2 并行分发。

### 2.2 触发迁移 Go 的条件（产品化信号）

- 路径档案从 `.sh` 升级为结构化格式（TOML），需要继承/校验/版本迁移；
- GUI：tray/图标角标/GNOME 设置面板形态的"一键开关"；
- 结构化错误、测试覆盖、多平台发布。

### 2.3 为什么是 Go 而非 Rust / Python

| 候选 | 判定 |
|---|---|
| **Go** | 瓶颈全在 bwrap 与应用本身，"薄封装"场景无极致性能需求；单静态二进制 + 交叉编译 + 成熟 `os/exec`/sysadmin 生态，与"封装 bwrap"的定位完全匹配 |
| Rust | 安全收益在此薄封装场景无法兑现，开发成本高，CLI/GUI 双视图迭代速度慢于 Go |
| Python | 部署是死穴：目标用户机器上解释器/依赖不可控，与"工具必须开箱即用"冲突 |

**一条红线**：bwrap 参数构造逻辑（`core` 包中的 `BuildBwrapArgs`，对应 bash 版
`build_bwrap_args`）是本项目的事实核心——它是 Android PMS"安装期目录分配"的
对等物。语言只是宿主；**直接 `os/exec` 调用 bwrap 二进制，绝不引入
containers/go 容器生态**（那会把方案拖向 podman 路线）。

## 3. 架构：单仓库，core 为心脏，CLI/GUI 为双视图

```
appclone/
├── core/               # 唯一核心：档案解析、实例 CRUD、bwrap 参数构造、discover 解析
├── cmd/appclone/       # CLI（cobra）——完整功能集，永久保留
├── cmd/appclone-gui/   # GUI（gotk4）——core 的另一个视图
├── profiles/           # 档案库（TOML），未来社区化分发
└── docs/
```

设计原则：

- **不搞"前后端分离"**：无 daemon、无 IPC 协议、无 REST。GUI 与 CLI 是同一
  core 包的两个薄视图，谁也不依赖谁；
- **CLI 永久保留**：核心用户可脚本化一切；GUI 崩溃不影响功能完整性；
- **core 为纯函数优先**：`BuildBwrapArgs(profile, instance) []string` 无副作用、
  可单测——这是整个项目测试价值的浓缩点。

## 4. GUI：gotk4 + libadwaita

### 4.1 为什么

- **形态匹配**：本应用的 UI 就是"设置面板"（应用列表 + 开关 + 实例管理），
  libadwaita 的 `AdwSwitchRow`/`AdwActionRow`/`AdwPreferencesWindow` 几乎是为
  "应用分身开关"这种交互原生设计的——与 GNOME「克隆应用」的官方形态同构；
- **无 cgo**：gotk4 经 purego/dlopen 加载 GTK4，编译期不需要 GTK 头文件，
  交叉编译不受 cgo 拖累；运行期依赖仅为 GTK4 + libadwaita（GNOME 系发行版默认具备，
  与 bwrap 可用人群高度重合）；
- **单二进制**：与 CLI/共享 core 的分发故事一致。

### 4.2 被否决的备选

| 备选 | 否决原因 |
|---|---|
| Wails / Electron（webview） | UI 仅数个列表页，浏览器壳纯属浪费——本项目拒绝"桌面前后端分离" |
| Fyne | 纯 Go 零运行时依赖是优点，但自绘 UI 在系统工具上无原生观感；留作无 GTK 环境时的备选 |
| Qt bindings (therecpe/qt 等) | Go 侧生态不成熟，维护风险高 |

### 4.3 已知摩擦与边界

- GTK 是 C 生态：事件回调与 Go 的 GC 生命周期需小心（闭包持有 widget）。
  本项目 UI 简单，刻意把交互限制在"列表 + 开关 + 对话框"内避免深水区；
  **若 UI 复杂到回调满天飞，视为设计走偏，而非换框架**。

### 4.4 GUI 范围（三个界面为止）

1. 应用列表页：扫描 `~/.local/share/applications`，每应用一行 + 分身开关
   （开 = `core.AddInstance`）；
2. 应用详情页：实例列表，启动/重命名/删除；
3. 首次配置对话框：`discover` 结果勾选界面——把"读 strace 输出"降级为
   "打勾确认哪些路径属于应用自身状态"，这是 GUI 的最大价值点。

**一版只做列表 + 开关；discover 勾选界面放二版。**

## 5. 数据格式与版本策略

- **档案格式**：v1 沿用 `.sh`（bash 加载）；v2 起 TOML
  （`profiles/<app>.toml`），含 schema 版本字段，Go 侧写迁移器一次性转换；
- **实例数据**：目录布局沿用方案文档 §4（`instances/<app>/<id>/root/<原路径>`），
  GUI 与 CLI、bash 与 Go 版本共用同一布局——**跨版本互操作优先于格式洁癖**；
- **删除语义**：一律回收站（gio trash），永久删除仅在回收站内提供。

## 6. 构建与分发

- **构建**：`go build`，无 cgo，`CGO_ENABLED=0`；GUI 二进制运行期要求 GTK4/libadwaita，
  在 README 标注而非打包时捆绑（发行版已有，捆绑反而制造冲突）；
- **分发渠道**（按优先级）：
  1. GitHub Releases：静态二进制（amd64/arm64）；
  2. 各发行版包（AUR / COPR / PPA）——外包给社区；
  3. bash 版随仓库分发（`curl | bash` 可用性）；
- **CI**：GitHub Actions——PR 触发 `gofmt`/`go vet`/`staticcheck`/`go test`，
  tag 触发 Release 构建；
- **许可证**：建议 GPL-3.0-or-later（与 GTK/libadwaita 生态一致性最好；
  core 为 MIT 亦可接受，若希望被第三方嵌入）。

## 7. 决策记录（ADR 索引）

| # | 决策 | 状态 |
|---|---|---|
| 001 | 隔离引擎选 bubblewrap，直接 exec 其二进制，不引入容器库 | 已定 |
| 002 | v1 bash 长期保留为最小依赖 fallback | 已定 |
| 003 | v2 实现语言选 Go，触发条件为产品化信号（GUI/结构化档案） | 已定 |
| 004 | 桌面应用不做前后端分离：CLI/GUI 共享 core，无 daemon | 已定 |
| 005 | GUI 选 gotk4 + libadwaita；Fyne 为无 GTK 环境备选 | 已定 |
| 006 | GUI 一版范围锁定"列表 + 开关"，discover 勾选放二版 | 已定 |
| 007 | 档案格式 v2 迁移 TOML；实例目录布局跨版本兼容 | 已定 |
