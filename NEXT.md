# NEXT

> 只回答三件事：现在在哪、下一步做什么、什么被挡住了。

## 现在在哪

设计基线已收敛：七轮只读审查累计 **84 条 findings**（14 + 12 + 6 + 10 + 5 + 6 + 15 + 16）全部修复。
第六轮之前查文字一致性；第七轮专查**机制缺口**（探测期删用户文件、孤儿进程、单例导致零写入、
`doctor --verify` 无归因手段、私有 DBus/runtime 无生命周期、状态目录无锁、运行态不可判定、
i18n 缺失、397 个应用无搜索、屏3 缺探测中态、公共目录组未落界面），15 条已全部落文档与设计稿；
随后复审又抓出 16 条（含 1 条 P0：**审计层漏了公共目录摘出，`--audit` 会删用户文件**），也已修完。
第八轮把回归 33 → 38 → **44 项**，脚本与交叉审查随即抓出 5 个真实行为缺陷（致命横幅单向死锁、入口标题错、
失败态重试点不动、open 入口取消不回弹、探测定时器泄漏改写 tab 高亮），已修并连续三次 44/44、退出码 0。
第九轮按"开工前把问题一次解决"又清了 12 项 P1，随后只读交叉审查又判 **REJECTED**（1 P0 + 12 P1），全部修完后回归 **44 → 58 项**，全绿退出码 0，7 组变异测试证明新断言有牙。
Go 实现未启动：`core/`、`cmd/`、`go.mod` 均不存在。
GUI 方向定案：**GTK4 + libadwaita 原生控件，经 puregotk 绑定（零 cgo）**，以 S0 spike 五条验收为切换闸门（第 5 条：Ubuntu 24.04 实机）。

## 下一步

按 `docs/方案设计.md` §10 路线图推进，探测先行：

0. **S0 spike**（半天，先把 GUI 绑定钉死）：30 行 puregotk 程序验五条——`AdwSwitchRow` 的 `notify::active` 进 Go 回调、
   `AdwNavigationView` push/pop + Esc、`AdwAlertDialog` 异步 response id、中文 + 系统暗色，
   以及**在 Ubuntu 24.04（libadwaita 1.5.0 / GTK 4.14.2）里跑通**（门槛定 1.5 的唯一依据）。不过就换 gotk4。
1. `core` 探测原语（overlay-diff + whiteout 收集 + 语义分类 → 生成档案）
2. `core` 其余部分（`BuildBwrapArgs` 纯函数 + 单测、档案 TOML 解析、实例 CRUD、`id`/`display_name` 校验）
3. CLI MVP（cobra）：`add/run/list/rm/doctor`
4. `cmd/appclone-gui`（puregotk + libadwaita 三屏，控件映射见 `tech-stack.md` §4.5）

## 被挡住的

- `fuse-overlayfs` 未安装、`/dev/fuse` 不存在：overlay 主探测与克隆起步无法验证。
  方案已定义降级判据（`docs/discover.md` §3.2/§3.4），可以先走 strace 分支开工；升级为 overlay 需装包，待用户确认。
- **GUI 侧的自动化闸门尚未存在**：`guitest.js` 只覆盖 HTML 稿。GTK 实现的回归要等 S0 之后建
  （`broadway`/`xdotool` 驱动三屏冒烟），在那之前 GUI 无回归保护，不能标 `verified`。

## 最近完成

- 2026-11-06 **第九轮：开工前的 12 项 P1 一次清掉**（用户要求"不要开始开发了才发现"）。判据统一为
  **"界面上承诺了 core 给不出的东西 / 状态机能进不能出 / 用户按一次就毁数据"**；`guitest.js` **44 → 58 项**，
  连续复跑 58/58、退出码 0，并用变异测试逐条确认新断言有牙：
  - **承诺 core 给不出的数据**：确认页写「路径 + 命中次数」、详情页写「结果完整（全部写入已捕获）」。
    overlay-diff 只能回答"写过/删过"，拿不到次数；没走到的功能分支根本不会出现在 upper 里。
    改为「观测到 N 个写入条目」与「覆盖本次会话实际发生的写入（没走到的分支查不到）」，
    并把这两条连同「已隔离 vs 共享」写成 §6.1 的**文案红线**表（禁写 / 改成 / 为什么）。
  - **产品首启死路**：空态把 `#listState` 整个 `display:none`，而引导语是"在下方「所有应用」里点亮开关"——
    用户被指向一个不存在的区域。改为空态只取代「已开启分身」那一组，搜索与「所有应用」照常；
    回归新增两条（空态下搜索与列表仍在、空态下开关**可见**且能走进首次配置）。注意"DOM 里有"不等于"看得见"，
    断言用 `offsetParent`。
  - **回车即删**：破坏性 `AdwAlertDialog` 把「移到回收站」设成默认响应。改为默认「取消」，
    稿子给取消按钮加 `.default` 标记，回归用 `page.keyboard.press('Enter')` 验证回车不删。
  - **确认到落地之间没有态**：写档案 + 建目录 + 生成 `.desktop` 是几百毫秒事务，连点两下建出两个实例，
    中途取消留半成品。新增 `creating` **第四态**：按钮「正在创建…」、确认与取消都禁用、Esc 也不生效
    （关闭路径统一在 `closeDialog` 里判 `creating`），回归断言"creating 期间重复提交仍只建一个实例"。
  - **档案失效被当成可复用**：入口原来只按"有没有实例/有没有档案"二分。拆 `has_profile` / `creatable`，
    加**第五个入口「重新配置 · X」**（重探 + 重建档案 + 建实例）。理由是复用失效档案会造出
    "什么都不隔离、启动即 `--bind` 源不存在"的分身；文档同步补 `profile_stale`（退出码 2）与
    `ValidateRunnable` / `ProfileState(app)` 两个 core 门禁。
  - **闸门失败文案混同**：`app_running` 是最高频首启失败，原来与"零写入"共用一句文案，用户会把它误诊成
    "这个应用不写盘"，然后反复重试。改为四类 `Reason` 四套文案（含"下一步做什么"），
    且闸门失败**连探测会话都不启动**（回归断言无 `.spinner`）。
  - **`shared_writes` 在界面上不可见**：用户在确认页看不到"最近文件 / 输入法状态是共享的"，
    遇到"两个分身最近文件混在一起"就判定产品坏了。新增第四组「共享但可写（不隔离）」只读组 + 代价文案，
    详情页档案组同步补一行。
  - **降级说明只在能被关掉的地方**：列表横幅「了解」一收，探测页里就没有任何"结果可能不完整"的提示了。
    改为 `envDegraded` 是**事实**、列表横幅只是**通知**：收起不影响探测页里那条（push 页盖住了窗口顶部横幅，
    所以探测页必须自带一条）。回归有专项断言。
  - **搜索逐键重建 400 行**：加 120ms 防抖 + 命中封顶 100 并提示"再输几个字"（与 §6.1.1 的 100 对齐，
    静默截断会被当成"这个应用不存在"）。
  - **副标题硬编码「1 个运行中」**：改用 `App.RunningN`；补 `App` 结构体到 §11（原文档 `ListApps` 返回 `[]App`
    却从没定义过它，GUI 拿不到 `Name[lang]`/`Icon`/`HasProfile`/`RunningN`，实现期必然自己再拼一套）。
  - **Esc 语义未定义 / 横幅可能同时两条**：Esc 定为三级（确认框 → 探测页 → 什么都不做，不导航）；
    致命与降级横幅互斥（bwrap 都没装时谈 fuse 降级没有意义）。
  - **toast 语义**：`AdwToastOverlay` 一次一条、新的顶掉旧的，不是队列。明确 toast 只做一次性确认，
    关键结果必须同时落在行内——用户没读到的那两秒等于没告诉过他。
- 2026-11-06 **第九轮交叉审查：REJECTED → 全部修完**（1 P0 + 12 P1 + 一批 P2）。这轮的价值在于：
  凡是"界面/文档承诺了实现给不出的东西"或"实现照着文档做会出事"的地方，都被挑出来了：
  - **P0 `creatable` 语义自相矛盾**：§6.1 写"`paths` **至少一条**还在盘上"，§11 的 `ValidateRunnable` 要求"**全部**存在"。
    按前者实现，"两条路径丢一条"的档案会被判为可复用、走「开启分身」跳过探测，`run` 时 bwrap 因缺失源整条失败——
    正是 `profile_stale` 要防的那起事故。统一为**全部存在才算 creatable**（缺一个源，整条 bwrap 命令就失败，不能"能用几条用几条"）。
  - **P1 界面泄漏内部标识**：启动 Toast 写「正在通过 bwrap 启动「微信 · work」」——引擎名 + 目录 id 一起摊给用户。
    改为只出现显示名；并给这条断言补文本校验（原来只断言"toast 出现过"，泄漏查不出来）。
    同类收口：删除按钮 tooltip 与关闭开关的正文不再提 `rm`/退出码 7；单例组不再向用户展示 `isolate_runtime = true`；
    组标题 `档案 · profiles.d/wechat.toml` → 「隔离设置」；实例根目录 → 「数据存放位置」。
  - **P1 要求用户开终端**：`env_unavailable` / `public_dir_leak` 的提示里写了「跑一次 appclone doctor」。
    `doctor` 是排障口，不是产品路径（AGENTS.md 零门槛红线）。改为「重新检查环境」+「反馈给支持」。
  - **P1 重命名不查重名**：只校验空值与 32 rune，同应用内能造出两个「分身 2」——GNOME 没有角标，两个入口看起来一模一样。
    补 NFC 归一、控制字符拒绝、同应用内唯一三项校验，并加回归。
  - **P1 克隆入口是死路**：telegram 有档案有实例，但候选数据缺它的条目，「新建分身」进去后确认按钮恒灰
    （门禁只数 config 项，一条都没有）。补数据 + 加断言「有档案且可创建的应用，克隆确认按钮必须可用」。
  - **P1 降级横幅误报**：`#dlgBanner` 只看 `envDegraded`，而 open/clone 复用档案、根本没有探测会话，
    却显示"本次走兼容模式"。改为只在真跑过探测（`PROBE_MODES`）时显示。
  - **P1 S0 验收写反了 Esc 语义**：`tech-stack.md` §4.3 第 1 条写"Esc 能返回"，与 §4.6 裁决（Esc 不承担返回）冲突。
    S0 若按错的语义验收，等于把 bug 验成通过。
  - **P1 CLI 与 GUI 入口没闭合**：`add --rediscover` 单独用会建实例，等于 GUI 的「重新配置」而不是「重新探测」。
    补 `add` × 档案状态 × 五入口的映射表，并点明二者只差 `--profile-only` 一个 flag。
  - **P1 `discover.md` 仍写三态/三组/"跟踪完整"**：实现者按它做会做出与 §6.1、§4.5 相反的界面。
    四态、四组、"N 个写入条目"、置信度不承诺完整，全部对齐。
  - **P2 一批**：`display_name` 默认值两处矛盾（"实例数+1" vs "最小未占用"）；`list --json` 的 `cloneable`
    与 `App.Creatable` 是同一个东西两个名字；discover 的公共目录清单漏了 `Desktop`（8 项里少一项）；
    顶层点目录 `~/.dingtalk` 在稿子里被默认勾选（真实 L2 是灰区、默认不勾——已在稿子与文档双双标注"别照抄"）；
    单例组固定出现 → 改为按 `app.singleton` 驱动；非破坏性提示框的默认响应没有可见标记；
    致命横幅解除后降级横幅不回来；副标题的"上次使用"取了 `instances[0]`（不一定是最近用过的那个，会误导）。
  - **回归 54 → 58 项**：新增「creating 期间 Esc/取消/点遮罩都关不掉」「重命名撞名被拒」「克隆入口确认可用」
    「候选行显示 N 个写入条目」，并把 6 条弱断言改硬（Tab 高亮改成"有且只有一个选中"、Toast 改成读文本、
    空态搜索框用 `offsetParent` 判可见、未开启行改成点真实应用行、降级横幅判可见性、运行数改走完整 `renderList` 路径）。
- 2026-11-06 **第八轮：5 个真实缺陷**（脚本暴露 4 条 + 交叉审查 1 条；都是设计稿行为 bug，不是文档口径问题；
  `guitest.js` 38 → **44 项**，连续三次 44/44、退出码 0）：
  - **致命横幅一开就关不掉**（最严重）：`document.click` 里 `body.blocked` 的短路排在状态演示按钮之前，
    打开"致命横幅"后连控制条自己也点不动 → 后续 11 条断言连锁失败。产品语义对应"环境恢复了但界面卡在不可用"——
    **状态机必须双向可达**，只能进不能出的状态与卡死是同一件事。
  - **入口标题把"配过但关掉了"叫成「首次配置」**：原实现按 `instances.length` 猜入口，导致有档案无实例的应用
    重开时既被告知"首次配置"（以为档案白做了），又白等一分钟探测。改为**四入口四标题**
    （`setup`/`open`/`clone`/`rediscover`），`open` 直接进确认态、跳过探测。
  - **失败态「重试」可能点不动**：`refreshConfirm()` 只在 confirm 分支调用，`error` 态沿用了上一轮的 `disabled`。
    失败态只有「重试」「取消」两条出路，点不动的重试等于让用户关窗口。
  - **`open` 入口取消后开关停在"开"（零实例）**：`closeDialog` 只在 setup/rediscover 重渲染列表。
    改为**一律按模型重渲染**——取消 = 什么都没发生。
  - **补两块零覆盖**：探测零写入 → 失败态（断言无 `[data-chk]`、不生成空档案、重试可点）；失败态重试 → 回到探测中。
  - **探测定时器泄漏**（交叉审查 P1）：`startProbe` 不先清旧定时器，探测中点两次屏3 tab 会留下第二个回调，
    1.2 秒后它照样执行、把已关闭的对话框重绘并把 tab 高亮改成屏3（可见的却是屏2）。改为 `stopProbeTimers()` 前置 +
    **epoch 编号**：取消/重入即作废旧回调——"取消 = 什么都没发生"要管到迟到的 `setTimeout`。
  - **第四入口无断言**：`guitest.js` 里 `#btnRediscover` 一条都没测过（STATUS 却写了"四种入口"。补一条：
    标题「重新探测」+ 按钮「保存档案」+ 实例数不变）。同时把"环境恢复"改成自包含断言（横幅收起 + 开关重新响应 +
    取消后开关回弹），把"公共目录"从 `>=7` 收紧为 `===8`，把"重试回声"补 `#overlay.open`。
    验证断言有牙：回退 epoch 保护后该条立刻变红（41/3、退出码 1）。
- 2026-11-06 **第七轮：15 条机制缺口全部落地**（方案 + 界面一起修，不只改文字）：
  - **P0 探测期会删用户文件**：overlay 罩住整个 `$HOME`，用户探测时存进 `~/Downloads` 的文件落在 upper，
    结束删 upper 就连带删掉。修法为「公共目录摘出」——在 `--bind merged $HOME` **之后**把 8 个 XDG 公共目录、
    `AC_BASE`、`shared_writes` 逐个 bind 回真实目录，并加**反向断言**：结束后 upper 里出现公共目录路径即作废本次结果。
  - **P0 孤儿进程**：`--die-with-parent` 只管直接子进程，应用 `fork`+`setsid` 出的进程逃得掉（bwrap 0.13.1 实测）。
    生命周期改由 `appclone run` 常驻父进程 + `setsid` + 整组 `SIGTERM`→3s→`SIGKILL` 保证，`--die-with-parent` 降为兜底。
  - **P0 单例导致零写入**：探测期固定全套隔离（私有 runtime + 私有 DBus + L0 空锁文件），
    并在开始前扫进程挡住"应用还开着"，四道闸门写进 `discover.md` §4。
  - **P0 关闭开关直接清空实例**：改为 `AdwAlertDialog` 二次确认，取消则开关回弹。
  - **P1 `doctor --verify` 无归因手段**：改成只在**有运行期审计层**时下结论（`run --audit` 复用探测的 overlay 原语），
    没有审计层输出 `unverified` 而不是"通过"；免 root 下 fanotify/eBPF/inotify 都不可行这条写进 ADR-019。
  - **P1 私有 runtime/DBus 生命周期**：`dbus-daemon --session --nofork --print-address` 由 `run` 起、与 bwrap 同生共死；
    缺 daemon 时降级告知。新增 §6.6。
  - **P1 状态目录无锁**：`AC_BASE/.lock`（flock，5s 超时，退出码 6）+ TOML 原子写 + `Mkdir` 的 `EEXIST` 判重（§4.2）。
  - **P1 运行态不可判定**：改用 `run.lock` 的 flock 试探（§4.3），不 pgrep、不 PID 文件；`AC_HOME` 补 `--setenv HOME`（§6.4）。
  - **P1 i18n / 397 个应用 / 屏3 缺探测中态 / 公共目录组未落界面**：`core/i18n` 消息表（zh_CN+en，不引 gettext）、
    搜索 + 前 30 行 + `cache/apps.toml`（ADR-024）、屏3 三态状态机、公共目录独立只读组。
  - **P2**：去掉 `--unshare-uts`（ADR-021）、档案 `version` 三种读法 + 未知字段保留（§5.5）、
    `.desktop` 的 `Name` 40 rune 上限与 `Icon`/`Categories` 缺省值（§4.1）。
  - **界面同步**：`design/guitest.js` 22 → **33 项**（新增探测中态、公共目录只读组、搜索过滤、两处确认框的
    "取消回弹"、运行中实例双禁用、致命横幅禁用开关、门禁只数应用自身数据），连续三次 33/33、退出码 0。
- 2026-11-06 **第七轮复审（16 条）**：后台两个 subagent 被中断，改用前台只读审查完成后逐条修：
  - **P0 审计层会删用户文件**：§6.5 的 `--audit` 用 overlay 罩住 `$HOME` 却没摘公共目录，
    用户审计期间存的文件落进 `audit/upper`，删 upper 即删用户文件。修法：审计层**强制复用探测期那份摘出清单**
    （`core` 单一实现，探测/审计共用），并把"① 摘出"写成 §6.5 的准入条件。
  - **P1 审计留存自相矛盾**：原文既说"运行结束即删"又说"verify 读上一次结果"，三条互相锁死。
    改为：运行结束**保留** upper → verify 消费后写 `audit/summary.toml` 再删 upper；`audit/` 从"必须自清理临时区"名单移出。
  - **P1 `verified` 判据错**：原写"upper 为空即通过"，但 overlayfs 对 `O_RDWR`/元数据变更都会 copy-up，
    会把正常行为误报成泄漏。改为"出现的路径是否都在 `paths`∪`shared_writes` 内"。
  - **P1 verify 旧口径残留**：§9 与 discover §7 仍承诺"常态报警"，改为"开启审计时才报警"。
  - **P1 §7 F 空断言**：先删 `probe/` 再查 `probe/upper` 永远为空。改为从 `add` 退出码看结论，并写清检查必须在清理之前。
  - **P1 设计稿五处**：可删除运行中实例（改为双禁用）、"已开启"组按 `supported` 分组（改为按实例数）、
    未开启应用没有开关（统一 SwitchRow）、缓存渲染进"单例处理"组（归回应用数据组）、
    创建门禁只数任意勾选（改为只数应用自身数据）、详情页让用户开终端（删除该文案）。
  - **P2**：退出码补 `8`（`leak-detected`）、`display_name` 默认值改为「分身 N」（原写"等于 id"与"用户不该看到 work"自相矛盾）、
    Banner 口径统一为"探测中起常驻"、探测期 `dbus-daemon` 回收补进 §3.2.3、`cache/apps.toml` 引用改 §6.1.1、
    序号引用改参数名、ADR 回引补齐、首次配置落点统一为"replace 成详情页"、致命横幅禁用开关、取消探测后 tab 高亮跟可见屏。
- 2026-11-06 **arm64 口径澄清**（用户问"跨平台是不是指支持 arm"）：`tech-stack.md` §6 把"平台边界"拆成
  「操作系统边界（只 Linux）」与「CPU 架构（amd64 + arm64，arm64 一等公民）」两条；依据是 purego README 的
  Tier 1 含 `linux/arm64`；补架构命名（deb `arm64` / rpm `aarch64` / Arch `aarch64`）、CI 矩阵排除
  `archlinux × arm64`、两个 GUI 冒烟 job（`ubuntu-24.04` 与 `ubuntu-24.04-arm`）、`.tar.gz` 命名带架构。
- 2026-11-06 **分发形态定案**（ADR-016 平台与产物、ADR-017 版本门槛，见 `tech-stack.md` §6）：
  - **操作系统只 Linux**（bwrap 是 Linux namespace 机制，macOS/Windows 原理不适用，不是待办）；
    **CPU 架构 amd64 + arm64，且 arm64 是一等公民**——purego 把 `linux/arm64` 列在 Tier 1，零 cgo 交叉编译可出包；
    但 arm64 必须用 `ubuntu-24.04-arm` runner 实跑 GUI 冒烟才算支持，只交叉编译不算；
    包格式架构名不同（deb `arm64` / rpm `aarch64` / Arch `aarch64`），Arch 的 arm64 走 AUR 源码包或 `.tar.gz`，
    `appclone-bin` 只含 x86_64；
  - 产物四件：`.deb`（Ubuntu 24.04+/Debian 13+）、`.rpm`（Fedora 41+ / CentOS Stream 10 / RHEL 10；openSUSE 包名体系不同，v1 不承诺）、
    `.pkg.tar.zst` + AUR `appclone-bin`（Arch，仅 x86_64）、裸 `.tar.gz` 兜底；deb/rpm/Arch 由 nfpm 一份
    `nfpm.yaml` 出，`.tar.gz` 由 CI 按同源清单另打（nfpm 没有 tarball packager）；
    依赖包名按格式 override，且**必须写版本下限**（裸包名在 Debian 12 / Leap 15.6 上也能装成功，然后 GUI init panic）；
  - **版本门槛从 libadwaita 1.6/GTK 4.16 下调为 1.5/4.14**：实测 Ubuntu 24.04 LTS 是 1.5.0/4.14.2，
    原门槛会把装机量最大的 LTS 排除；代价仅 `AdwSpinner` 换 `GtkSpinner`。生效前提是 S0 第 5 条（24.04 实跑）；
  - **新发现的阻断前提**：Ubuntu 23.10+ 默认 `kernel.apparmor_restrict_unprivileged_userns=1`，而 Ubuntu 的
    bubblewrap 包不带 AppArmor profile（Launchpad #2144531）→ 裸装 bwrap 直接 `setting up uid map: Permission denied`。
    解法：postinstall 检测到该 sysctl 为 1 时写 `/etc/apparmor.d/bwrap`（`userns,`）并 reload；`doctor` 增加该检查；
  - 包内落点已定：`/usr/bin/{appclone,appclone-gui}`、`/usr/share/applications/io.github.mensylisir.Appclone.desktop`
    （文件名必须等于 app id）、hicolor 图标；postinstall 另跑 `update-desktop-database` + `gtk-update-icon-cache`；
  - 明文不做 Flatpak/Snap/AppImage/COPR/PPA，"外包给社区"的说法已删（空头承诺）。
- 2026-11-06 **第三轮交叉审查后收口**（6 条）：`Name` 后缀统一为显示名（`id` 只进路径与 `X-Appclone-Id`）；
  §6.3"探测中途退出"改为清理探测临时区（原写"保留 upper 层"，与清理约定冲突）；
  `probe_run` 的 `trap` 改用字面路径（`EXIT` 触发时 `local` 变量已失效，原写法清不掉）；
  设计稿数据模型改 `id` + `display_name`，重命名只改显示名；socket 示例路径改为 `$XDG_RUNTIME_DIR/`。
- 2026-11-06 **第二轮交叉审查后收口**（12 条 findings 全部修复）：
  - 实例 `id` 与显示名分离：新增 `instances/<app>/<id>/instance.toml`（`display_name`/`created_at`/`probe_method`）；
    重命名只改 `display_name`，`id` 创建后不可变；`.desktop` 的 `Name=` 用显示名、路径用 `id`。
  - `add --fork` 目录寿命写清：`root/<原路径>/` 是持久 upper 层，`.tmp-work/`、`.tmp-merged/` 才可删——
    此前文档把它们混成"临时区"，照做会删掉用户数据。
  - `probe_run` 加 `trap ... EXIT INT TERM` 统一卸载与清理，与"临时区残留即 bug"的约定对齐。
  - keyring 从 `shared_writes` 移出、单列 `unsupported`（`shared_writes` 是 `doctor --verify` 的预期共享白名单，混进去会让校验口径失真）。
  - 设计稿里"单例 sock → 空文件覆盖"的**错误文案**改为 `isolate_runtime`（原写法会 ENOTSOCK，分身起不来）。
  - 删除确认登记为"原生比稿子多"的唯一交互；`ADR-011` 与 §6 命令表统一为 id 二分规则；版本门槛统一 GTK 4.16 / libadwaita 1.6（同日晚些下调为 4.14/1.5，见 ADR-017）。
- 2026-11-06 GUI 方案定稿（保持 GNOME 原生），首轮交叉审查 14 条已修：
  - `tech-stack.md` §4 重写：绑定 gotk4 → **puregotk**（GTK4+Adw 原生 + `CGO_ENABLED=0` 可构建），
    gotk4 降兜底；补三屏控件映射、与 HTML 稿的裁决表、线程模型（探测 goroutine + `IdleAdd`）；
    新增 ADR-013（原生控件 + 稿子只是交互基线）、ADR-014（不引 GSettings，用 `ui.toml`）、ADR-015（v1 不做 Flatpak）。
  - `方案设计.md`：新增 §4.1 `.desktop` 字段规范（`Exec` 绝对路径 + `TryExec`，`%U` 两种相反语义）、
    §6.1 控件级主流程 + 三条 UI 约束、`doctor` 并入 GTK 检查（仅开发者排障口，不进产品路径）。
  - `design/guitest.js`：自动发现 Chromium/puppeteer-core、`pathToFileURL`、**失败非 0 退出码**；
    四次复跑均 22/22、退出码 0（含 HTML 文案、样式与数据模型改动之后）。
- 2026-11-06 方案体检后修订三份文档（14 项）：bwrap 参数契约（`--unshare-*`、挂载顺序）、共享可写文件、
  失败语义、三目录应用来源（系统级 397 vs 用户级 12）、`doctor --verify` 拆分、`<id>` 规则、
  去掉私有 `.trash/`、验证清单补反向断言、L0 平台前提、两轮探测。
