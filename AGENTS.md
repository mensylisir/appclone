# appclone Agent 工作规则

## 项目上下文

appclone 是把 Android「应用分身 / Cloned Apps」语义移植到 Linux 桌面的工具：**只隔离目标应用自身的配置与数据目录**（per-app 沙箱，两实例互不可见），**其余一切照常共享**（公共文件、DBus 会话、外设）。无需 root、无需修改应用本身。当前仓库处于 v1 bash 可用、v2 Go 重构与 GUI 未启动阶段。

每次开始工作前按顺序读取：

1. `docs/方案设计.md`：唯一设计契约基线——需求定义、目录布局、档案规范、CLI 命令面、验证清单，跨版本（bash/Go、CLI/GUI）稳定。
2. `docs/tech-stack.md`：唯一技术选型与 ADR 基线，ADR-001~008 已定案。
3. `docs/discover.md`：路径自动探测方案谱系与三层管线（ADR-008 详情）。
4. `design/appclone-gui.html`：**GUI 界面行为基线**。任何 GUI 相关改动（新增屏、改组件、调交互、改状态变体）之前必须读；改后必须跑 `node design/guitest.js`（22 项交互回归，需 chromium + puppeteer-core），退出码非 0 视为设计基线被破坏。

冲突裁决：交互行为与组件映射以 `design/appclone-gui.html` 为准（含窗口下方的组件映射注释卡）；目录布局、档案语义、CLI 命令面以 `docs/方案设计.md` 为准；选型冲突以 `docs/tech-stack.md` ADR 为准。

不要创建阶段总结、交付报告、重复设计文档或新的进度文件。

## 状态与文档

- 设计变更（目录布局、档案规范、命令面）：更新 `docs/方案设计.md`，并同步核对 `docs/tech-stack.md` 是否需要追加 ADR。
- 选型或架构决策变更：先在 `docs/tech-stack.md` 追加 ADR 条目，再改实现。
- 探测/档案机制变更：更新 `docs/discover.md`。
- 文档互引必须保持一致：改名或新增文档时同步更新所有引用处。
- 及时提交推送到 `git@github.com:mensylisir/appclone.git`（SSH remote，main 分支，每任务一次提交）。

## 技术栈（已定案，ADR-001~008）

- 隔离引擎：bubblewrap（直接 exec 其二进制，**禁止引入容器库/改走 podman 路线**）。
- 路径探测：主用 overlay-diff（fuse-overlayfs，lower=$HOME 只读 / upper=空写层）；strace 降级兜底；静态预填白拿 AppArmor profile 与 Flatpak 元数据。
- 克隆起步：fuse-overlayfs（lower=主实例数据，无 root CoW）。
- v1：bash（长期保留为最小依赖 fallback，与 v2 并行分发）。
- v2：Go ≥1.22 单仓库，`core` 包为唯一事实核心（`BuildBwrapArgs` 纯函数），CLI（cobra）与 GUI（gotk4 + libadwaita）是 core 的两个薄视图；**无 daemon、无 IPC、无 webview**。
- 档案格式：v1 `.sh`，v2 迁 TOML，字段语义逐项对应（见方案设计 §5.3）；实例目录布局跨版本不变。

## 工程原则

1. **核心语义不可破**：只隔离 `AC_PATHS` 声明的目录，`--bind / /` 全透传——任何改动若导致公共目录（Downloads/Documents 等）被隔离，即背叛项目立意。
2. **免 root 红线**：任何方案引入特权要求（fanotify 归因、eBPF、真实 mount）前必须先否决或单列授权说明。
3. **复杂度不泄漏给用户**：产品标准是「像安卓设置里一个开关」——路径探测、锁文件处理、DBus 隔离等复杂度必须由工具消化；用户界面最多出现"确认预勾选项"级别的操作。
4. `BuildBwrapArgs` 等核心逻辑必须保持纯函数可单测；bash 与 Go 版本行为等价，命令面与目录布局是跨版本契约。
5. 探测/运行会产生用户数据写入——所有验证用隔离的假 `$HOME`（临时目录 + `HOME=`/`AC_BASE=` 重定向），不得污染真实主目录。
6. 删除实例一律回收站（gio trash），禁止 `rm -rf` 用户数据目录。
7. **禁止死等，主动检查**：不阻塞等待长任务；strace 探测等交互式过程由用户手动结束，工具只做前后即时检查。

## 每个工作包完成定义

一个功能只有同时满足以下要求才算完成：

1. **回归通过**：改 `design/appclone-gui.html` 必须跑 `node design/guitest.js`（22/22）；改 bash 脚本必须过 `bash -n` 且在隔离假 HOME 下完成功能冒烟（写入落实例目录、宿主零污染、公共目录共享可见）。
2. **契约不破**：CLI 命令名、实例目录布局、档案字段语义三者在改动前后保持兼容（bash/Go/CLI/GUI 视角一致）。
3. **文档同步**：涉及机制变更时同步更新对应 docs 并保持互引一致。
4. **状态真实**：不使用「已完成」描述没有验证证据的功能；验证输出（命令、结果）写进提交说明或 docs。

## Subagent 交叉审查（强制）

每次有功能或任务完成（新命令、缺陷修复、探测机制变更、GUI 屏改动、v2 core 重构、发版前），必须在标记完成前通过 `Agent` 工具启动 subagent 审查：

1. **标准审查**：只读 subagent，提供 docs 基线 + 变更文件清单 + 验证证据，确认实现与契约一致。
2. **对抗性审查**（高风险变更必须）：探测逻辑、bwrap 参数构造、锁文件/DBus 处理、实例数据删除路径——专门挑边界情况（路径含空格/中文、HOME 位置异常、fuse 不可用降级、同名实例）。

要求：审查 subagent 只读不改；输出 APPROVED/REJECTED + findings（严重级别 + 文件:行号）；确认问题修复或记录不修复理由后才算完成；subagent 不可用时明确记录阻塞，不得静默跳过。

## 安全要求

1. 不提交、打印用户应用数据内容（聊天记录、账号态属于分身数据，验证时只检查文件存在性）。
2. 生成的 bwrap 命令不得引入越权路径；`AC_PATHS` 外的 bind 需要档案显式声明（`AC_EXTRA_BINDS`）。
3. 探测会话运行用户应用属正常行为，但工具自身不得在探测范围外写用户目录。
4. GUI/CLI 的删除操作只移入回收站；永久删除仅能由用户手动在回收站完成。

## 代码与提交

1. 仓库布局铁律：**根目录只放可执行脚本 `appclone`**；文档一律 `docs/`；设计稿与测试 `design/`；v2 代码按 `core/`、`cmd/appclone/`、`cmd/appclone-gui/`、`profiles/` 组织。
2. bash 脚本保持 POSIX 可读性，`set -euo pipefail`，不引入 bash 之外解释器依赖。
3. 提交信息用中文，说明「为什么」而非罗列文件；一个任务一个 commit。
4. 不要未经用户确认创建 git commit、分支、tag 或发布。
5. **push 默认放行**：一次已确认的 commit 完成后，推送当前分支到 `origin` 不再单独征求确认。仍需逐次确认的是：`--force`/`--force-with-lease`、推到非当前分支或 tag、发布（release）以及任何改写已推送历史的操作。
6. **提交署名**：以用户本人 git 身份提交（`git config user.name` / `user.email` 为准），禁止使用 Claude、AI 或任何其他名义署名，不得添加 AI 署名 trailer。
7. 远端使用 SSH：`git@github.com:mensylisir/appclone.git`（本机 HTTPS 无凭据，push 会失败）。

## 常用命令

```bash
bash -n appclone                          # bash 语法检查
node design/guitest.js                    # GUI 设计稿 22 项交互回归
# 隔离冒烟（假 HOME，不污染真实主目录）：
TESTROOT=$(mktemp -d); export AC_BASE="$TESTROOT/xdg/appclone" XDG_DATA_HOME="$TESTROOT/xdg"
HOME="$TESTROOT/home" ./appclone profile-new t && HOME="$TESTROOT/home" ./appclone add t main
HOME="$TESTROOT/home" ./appclone run t main -c 'echo ok > "$HOME/.config/t/x"'
find "$AC_BASE/instances" -type f        # 数据应只落在实例目录
```
