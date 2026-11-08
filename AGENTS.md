# appclone Agent 工作规则

## 项目上下文

appclone 是把 Android「应用分身 / Cloned Apps」语义移植到 Linux 桌面的**产品**：**只隔离目标应用自身的配置与数据目录**（per-app 沙箱，两实例互不可见），**其余一切照常共享**（公共文件、DBus 会话、外设）。无需 root、无需修改应用本身。目标用户是普通桌面用户，产品标准是「像安卓设置里一个开关」——用户不接触命令行、档案文件、探测细节。当前仓库处于设计基线完备、Go 实现未启动阶段。

每次开始工作前按顺序读取：

1. `NEXT.md`：当前进度和下一步。
2. `docs/方案设计.md`：唯一设计契约基线——需求定义、目录布局、档案规范、CLI 命令面、验证清单，跨版本（bash/Go、CLI/GUI）稳定。
3. `docs/tech-stack.md`：唯一技术选型与 ADR 基线（编号以该文 §7 索引为准）。
4. `docs/discover.md`：路径自动探测方案谱系与三层管线（ADR-008 详情）。
5. `design/appclone-gui.html`：**GUI 界面行为基线**。任何 GUI 相关改动（新增屏、改组件、调交互、改状态变体）之前必须读；改后必须跑 `node design/guitest.js`（交互回归，项数以脚本内 `EXPECTED` 为准，当前 62 项，需 chromium + puppeteer-core），退出码非 0 视为设计基线被破坏。
6. `docs/STATUS.md`：已验证功能状态。

冲突裁决（三份各管一段，不要互相越界）：

- **交互行为**（点了什么、状态怎么变）以 `design/appclone-gui.html` 为准；
- **组件映射与视觉**以 `docs/tech-stack.md` §4.5/§4.6 为准——HTML 稿是浏览器画的，不是 GTK 渲染的，照抄必然不原生（ADR-013 已裁决，稿子内那张映射卡是 v1 遗留，以 §4.6 裁决表为最终口径）；
- **目录布局、档案语义、CLI 命令面**以 `docs/方案设计.md` 为准；
- **选型冲突**以 `docs/tech-stack.md` §7 ADR 索引为准（编号会增补，别在别处硬写范围）。

不要创建阶段总结、交付报告、重复设计文档或新的进度文件。

## 项目规范

开始任何开发任务前，必须阅读并遵循以下规范文件：

| 文件              | 职责                                             | 要求     |
| ----------------- | ------------------------------------------------ | -------- |
| `ENV.md`          | 开发环境、语言版本、工具链、运行时和依赖环境     | 必须遵循 |
| `ARCHITECTURE.md` | 软件架构、模块划分、目录组织、依赖关系和设计原则 | 必须遵循 |
| `AGENTS.md`       | Agent 工作流程、行为约束、代码修改和交付要求     | 必须遵循 |

### 执行要求

1. 开始开发前，检查以上文件是否存在并完整阅读。
2. 严格遵循 `ENV.md` 中的语言版本和工具链要求，禁止擅自使用系统自带运行时替代项目指定版本。
3. 严格遵循 `ARCHITECTURE.md`，根据项目实际类型和复杂度设计代码结构，不得机械套用架构模板。
4. 优先遵循项目已经建立且合理的架构和代码约定。
5. 如果规范之间存在冲突，先分析冲突原因，不得擅自进行重大架构调整。
6. 新增模块、修改模块边界或调整依赖关系时，必须检查架构合理性。
7. 重大架构调整应先向用户说明设计方案。
8. 完成任务后检查代码结构、依赖关系、测试结果，并在必要时更新架构文档。

不得仅为了完成当前功能而忽略项目整体结构和长期可维护性。

## 状态与文档

- 设计变更（目录布局、档案规范、命令面）：更新 `docs/方案设计.md`，并同步核对 `docs/tech-stack.md` 是否需要追加 ADR。
- 选型或架构决策变更：先在 `docs/tech-stack.md` 追加 ADR 条目，再改实现。
- 探测/档案机制变更：更新 `docs/discover.md`。
- 功能状态：只根据测试证据更新 `docs/STATUS.md`。
- 会话进度：每次结束前更新 `NEXT.md`。
- `planned` 表示只有计划；`verified` 必须有命令、日期、结果和覆盖范围。
- 文档互引必须保持一致：改名或新增文档时同步更新所有引用处。
- 及时提交推送到 `git@github.com:mensylisir/appclone.git`（SSH remote，main 分支，每任务一次提交）。

## 技术栈（ADR 索引见 `docs/tech-stack.md` §7）

- 隔离引擎：bubblewrap（直接 exec 其二进制，**禁止引入容器库/改走 podman 路线**）。
- 路径探测：主用 overlay-diff（fuse-overlayfs，lower=$HOME 只读 / upper=空写层）；strace 降级兜底；静态预填白拿 AppArmor profile 与 Flatpak 元数据。
- 克隆起步：fuse-overlayfs（lower=主实例数据，无 root CoW）。
- 实现：**Go ≥1.22 唯一路径**（ADR-002/003：不存在 bash 产品版）。`core` 包为唯一事实核心（探测管线、`BuildBwrapArgs` 纯函数、档案 TOML、实例 CRUD），CLI（cobra，开发者视图）与 GUI（puregotk → GTK4 + libadwaita 原生控件，**普通用户主入口**）是 core 的两个薄视图；**无 daemon、无 IPC、无 webview**。
- 档案格式：TOML（运行时 `~/.local/share/appclone/profiles.d/<app>.toml`，仓库 `profiles/` 只是只读模板库），由探测管线自动生成，用户只做勾选确认；实例目录布局 CLI/GUI 一致。

## 工程原则

1. **核心语义不可破**：只隔离档案 `paths` 声明的目录，`--bind / /` 全透传——任何改动若导致公共目录（Downloads/Documents 等）被隔离，即背叛项目立意。
2. **免 root 红线**：任何方案引入特权要求（fanotify 归因、eBPF、真实 mount）前必须先否决或单列授权说明。
3. **复杂度不泄漏给用户**：产品标准是「像安卓设置里一个开关」——路径探测、锁文件处理、DBus 隔离等复杂度必须由工具消化；用户界面最多出现"确认预勾选项"级别的操作。
4. `BuildBwrapArgs` 等核心逻辑必须保持纯函数可单测；bash 与 Go 版本行为等价，命令面与目录布局是跨版本契约。
5. 探测/运行会产生用户数据写入——所有验证用隔离的假 `$HOME`（临时目录 + `HOME=`/`AC_BASE=` 重定向），不得污染真实主目录。
6. 删除实例一律回收站（gio trash），禁止 `rm -rf` 用户数据目录。
7. **禁止死等，主动检查**：不阻塞等待长任务；strace 探测等交互式过程由用户手动结束，工具只做前后即时检查。

## 每个工作包完成定义

一个功能只有同时满足以下要求才算完成：

1. **回归通过**：改 `design/appclone-gui.html` 必须跑 `node design/guitest.js`（全过，项数以 `EXPECTED` 为准）；改 Go 代码必须过 `go vet` + `go test ./...`，涉及 bwrap/探测行为时在隔离假 HOME 下完成功能冒烟（写入落实例目录、宿主零污染、公共目录共享可见）；改 `cmd/appclone-gui` 还必须跑 `design/guitest-gtk.sh`（真实 GTK 闸门，需显示 + 宿主 `bwrap`）。
2. **契约不破**：命令语义、实例目录布局、档案字段语义在改动前后保持兼容（CLI/GUI 视角一致）。
3. **用户零门槛红线**：任何把"要求用户读 strace 输出 / 手写档案字段 / 开终端"的设计引入产品路径，一律打回——这是本产品与脚本工具的分界线。
4. **文档同步**：涉及机制变更时同步更新对应 docs 并保持互引一致。
5. **状态真实**：不使用「已完成」描述没有验证证据的功能；验证输出（命令、结果）写进提交说明或 docs。

## Subagent 交叉审查（强制）

每次有功能或任务完成（新命令、缺陷修复、探测机制变更、GUI 屏改动、v2 core 重构、发版前），必须在标记完成前通过 `Agent` 工具启动 subagent 审查：

1. **标准审查**：只读 subagent，提供 docs 基线 + 变更文件清单 + 验证证据，确认实现与契约一致。
2. **对抗性审查**（高风险变更必须）：探测逻辑、bwrap 参数构造、锁文件/DBus 处理、实例数据删除路径——专门挑边界情况（路径含空格/中文、HOME 位置异常、fuse 不可用降级、同名实例）。

要求：审查 subagent 只读不改；输出 APPROVED/REJECTED + findings（严重级别 + 文件:行号）；确认问题修复或记录不修复理由后才算完成；subagent 不可用时明确记录阻塞，不得静默跳过。

## 安全要求

1. 不提交、打印用户应用数据内容（聊天记录、账号态属于分身数据，验证时只检查文件存在性）。
2. 生成的 bwrap 命令不得引入越权路径；档案 `paths` 之外的 bind 需要档案显式声明（`extra_binds`）。
   （`AC_PATHS`/`AC_LOCKFILES` 是 `legacy/` bash 原型的环境变量名，**产品代码与文档一律用 TOML 字段名**。）
3. 探测会话运行用户应用属正常行为，但工具自身不得在探测范围外写用户目录。
4. GUI/CLI 的删除操作只移入回收站；永久删除仅能由用户手动在回收站完成。

## 代码与提交

1. 仓库布局铁律：**根目录不放任何产品代码**；Go 代码按 `core/`、`cmd/appclone/`、`cmd/appclone-gui/`、`profiles/` 组织；文档一律 `docs/`；设计稿与测试 `design/`；早期 bash 原型归档 `legacy/`（不发布、不承诺维护，仅供 core 开发期行为对照）。
2. Go 代码遵守 `gofmt`/`go vet`/`staticcheck`；`core` 保持纯函数优先、可单测。
3. 提交信息用中文，说明「为什么」而非罗列文件；一个任务一个 commit。
4. **commit + push 默认放行**：一个任务做完就直接提交并推送到 `origin` 当前分支，**不要逐次征求确认**（用户在 2026-11-07 明确要求：每次改动不仅 commit，还要 push）。仍需逐次确认的是：`--force`/`--force-with-lease`、推到非当前分支、打 tag、发布（release），以及任何改写已推送历史的操作。
   - 这里的"做完"指：验收命令已跑过并留下证据（回归全过（项数以 `EXPECTED` 为准）、退出码 0；Go 侧 `go vet` + `go test`）。没跑过验证就提交，等于把未验证状态当成果。
   - 提交前 `git status` 自查：不要把 `.workbuddy/`（本地工作区数据）带进提交，它已在 `.gitignore` 里。
6. **提交署名**：以用户本人 git 身份提交（`git config user.name` / `user.email` 为准），禁止使用 Claude、AI 或任何其他名义署名，不得添加 AI 署名 trailer。
7. 远端使用 SSH：`git@github.com:mensylisir/appclone.git`（本机 HTTPS 无凭据，push 会失败）。

## 常用命令

```bash
gofmt -l . && go vet ./... && go test ./...    # Go 门禁
go vet -tags gtk_e2e ./cmd/appclone-gui        # 闸门测试文件也要能编译
staticcheck ./...                              # CI 阻塞门禁；本地先 go install honnef.co/go/tools/cmd/staticcheck@v0.8.2（对应 2026.2.2）
CGO_ENABLED=0 go build ./...                   # 全仓零 cgo（puregotk 不需要 C 工具链）
# GUI 交互回归（项数以脚本内 EXPECTED 为准，当前 62 项；退出码 0 才算过；脚本自动发现 chromium 与 puppeteer-core）
node design/guitest.js
# 找不到依赖时：npm i puppeteer-core，或用 PUPPETEER_CORE=/path、CHROMIUM=/path 指定
# GTK 侧回归（真实 GTK，需 DISPLAY/WAYLAND + 宿主 bwrap；假 HOME，18 条约 6 秒）
design/guitest-gtk.sh                          # --keep 保留临时目录与假 HOME 便于排查
# 隔离冒烟（假 HOME，不污染真实主目录）：
TESTROOT=$(mktemp -d); export AC_BASE="$TESTROOT/xdg/appclone" XDG_DATA_HOME="$TESTROOT/xdg"
HOME="$TESTROOT/home" ./appclone add t main    # Go CLI 建成后替换为 go run ./cmd/appclone
find "$AC_BASE/instances" -type f              # 数据应只落在实例目录
```
