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
**第十轮：Go 开发已启动**（2026-11-07）。`go.mod` + `core/`（errors/paths/profile/validate/bwrap/state/instance/apps/launcher/run/trash/i18n/bundled）+ `cmd/appclone`（add/run/list/rm/doctor/repair-desktop/version）+ `profiles/`（内置模板，`//go:embed`）。
`go vet ./...` 干净、`go test ./...` 全绿；假 HOME 冒烟跑通 add→list→doctor→rm。
bwrap 参数语义用 `doctor` 打印的 argv 实测过 A/A'/B（实例内落盘、宿主零污染、公共目录共享），**但只在 root 下手工执行**，`run` 本身拒绝 root。
GUI 方向定案：**GTK4 + libadwaita 原生控件，经 puregotk 绑定（零 cgo）**，以 S0 spike 五条验收为切换闸门（第 5 条：Ubuntu 24.04 实机）。
**第十一轮：实现审查收口**（2026-11-07）。3 个 P0（bwrap 私有目录的 bind 源落进 `root/`、`run` 覆盖 `RuntimeDir`、
`paths` 未做 `EvalSymlinks`）与主要 P1 修完并推送；本轮又清掉删除白名单、回滚链、GTK 版本探测三条，
`gofmt`/`go vet` 干净、`go test ./...` 全绿（新增 8 个断言）。

## 下一步

路线图第 1 项（`core` 探测原语）**被本机环境挡住**：`fuse-overlayfs 1.18` 与 `fusermount3 3.18.3` 已装（用户确认），
但容器内没有 `/dev/fuse` 设备节点（`mknod` 被沙箱拒），`fuse-overlayfs` 挂不起来。
所以探测要拆成两半，先后顺序按"现在能不能验证"排：

1a. **可离线验证的部分**（**已完成** 2026-11-07）：`MergeUpper`（upper 归并：取应用目录/单文件本身、
   剔除 XDG 根、whiteout 单列、按路径稳定排序）+ `Classify`（§5 有序规则表）+ `ProfileFromCandidates`
   （paths / shared_writes / unsupported 三落点互斥）→ `core/classify_test.go` 8 组断言。
1b. **需要真实会话的部分**：overlay 会话编排**已实现**（`core/probe.go`，待真机验证）；
    strace 降级与"两轮探测循环"仍未做。

其余路线图项（编号沿用 `docs/方案设计.md` §10）：

0. **S0 spike**（半天，先把 GUI 绑定钉死）：30 行 puregotk 程序验五条——`AdwSwitchRow` 的 `notify::active` 进 Go 回调、
   `AdwNavigationView` push/pop + Esc、`AdwAlertDialog` 异步 response id、中文 + 系统暗色，
   以及**在 Ubuntu 24.04（libadwaita 1.5.0 / GTK 4.14.2）里跑通**（门槛定 1.5 的唯一依据）。不过就换 gotk4。
1. `core` 探测原语（1a + 1b）
2. ~~`core` 其余部分~~ **已完成**（`BuildBwrapArgs` 纯函数 + 单测、档案 TOML、实例 CRUD、`id`/`display_name` 校验）
3. ~~CLI MVP~~ **已完成**（`add/run/list/rm/doctor/repair-desktop/version`）
4. `cmd/appclone-gui`（puregotk + libadwaita 三屏，控件映射见 `tech-stack.md` §4.5）。
   **前置**：拉 `puregotk` 依赖需要网络（本机 `proxy.golang.org` 502），且 S0 五条未做。

## 被挡住的

- **开发沙箱里没有 `/dev/fuse`**（`mknod` 被拒、节点不存在）：L1 的 syscall 部分（真挂载 + 真会话）**在这里
  无法验证**。用户机器上是有的（`ls /dev/fuse` 正常），所以这不是产品限制，只是我这边的环境限制。
  验证办法：在真机跑 `design/probe-smoke.sh /path/to/appclone`——它用假 HOME 走完整条链路
  （doctor → add 触发探测 → 检查档案里的 `ProbeDemo` 路径 → 检查 `/var/tmp` 无残留 → 检查假 HOME 零污染 → rm）。
- **降权验证同样被挡**：`runuser`（cannot set groups）、`setpriv`（setresuid failed）、`chown`（Invalid argument）
  全部被沙箱拒绝，所以 bwrap 语义只能在 root 下手工执行，`run` 的非特权路径至今没有实机证据。
- **GUI 侧的自动化闸门尚未存在**：`guitest.js` 只覆盖 HTML 稿。GTK 实现的回归要等 S0 之后建
  （`broadway`/`xdotool` 驱动三屏冒烟），在那之前 GUI 无回归保护，不能标 `verified`。

## 最近完成

- 2026-11-07 **第十八轮：对抗性审查判 REJECTED 后的修复 + 按职责拆文件**：
  - **文件粒度**：`probe.go` 一度写到 907 行（把纯函数、syscall、生命周期、清理全塞一起），
    违反 `AGENTS.md` 的"core 保持纯函数优先、可单测"。拆成 `probe.go` 246 / `probe_args.go` 219 /
    `probe_overlay.go` 168 / `probe_session.go` 154 / `probe_cleanup.go` 121 / `proc.go` 47 行。
  - **P0-1 探测产物没有 `exec`**：`ValidateProfile` 直接拒 → `add` 走探测后 100% 失败（上一轮的
    "现在真的能配出来"是错的）。改为探测时回填 `Exec/ExecArgs`，并加端到端断言。
  - **P0-2 首次配置死循环**：`CreateInstance` 用 `ValidateRunnable` 要求宿主路径已存在，而探测只写
    overlay 的 upper → 探测成功也报"档案已失效"，重新探测还是同一结果。拆出 `ValidateBindable`
    （建实例只要求 Exec/extra_binds，路径由 `prepareBindTargets` 补建），`ValidateRunnable` 只留给 `run`。
  - **P0-3 quarantine 失败会删现场**：`os.Rename` 失败后回退到 `RemoveAll`——那里面可能就是用户刚存的
    文件。改成保留 + 报错，名字加 uid/时间/随机尾巴防撞名。
  - **P0-4/P0-5 公共目录**：不再无条件 `--bind`（缺一个公共目录整条命令就失败），只 bind 存在的；
    并且**不再回退八个英文默认名**——读不到 `user-dirs.dirs` 就禁用探测（3）。
  - 其余：污染退出码改 4（原 3）、应用在跑改 6（原 7）、会话起不来 3（与"零写入"区分）、
    超时/取消不产出档案、卸载失败不删工作目录、`noatime`（防伪 copy-up）、宿主 `bus` 不进探测 runtime、
    污染检查纳入 AC_BASE 与 shared 并改用软链容错比较、`add` 接管 SIGINT。
  - 仍未做（记在 discover.md 的已知局限）：会话内**正向断言**（写标记文件回读）、
    strace 降级、两轮探测循环。
- 2026-11-07 **第十七轮：L1 overlay 探测落地（待真机验证）**：
  - `core/probe.go`：四道闸门、`ProbeBwrapArgs` 全序、会话生命周期、污染反向断言、quarantine 保留现场。
    几个关键决定：`fuse-overlayfs` 用 `-f` 前台跑（拿得到句柄才能确定它真的退出了）+ `auto_unmount`
    （core 被 SIGKILL 时内核帮忙卸载，否则下次 EBUSY）+ `Pdeathsig`；`instances/` 在探测期用 tmpfs 盖住
    （不该看见也不该写已有分身）；宿主 socket 要**重定向进私有 runtime**（只 bind 原位等于没 bind）。
  - `add` 现在对没有内置模板的应用**真的会探测**：启动一次应用、用一下、关掉，然后归因。零写入 → 退出 4
    且不生成空档案；污染 → 退出 3 + `quarantine-*` 现场。
  - **可测与不可测分界写清**：argv、污染判定、编排（注入假 mounter/runner）全部单测；真挂载/真会话
    在沙箱里没有 `/dev/fuse`，**未验证**，交给 `design/probe-smoke.sh` 在真机跑。
- 2026-11-07 **第十六轮：第二轮复审（REJECTED）的三处 P1 修完**：
  - `blanket_path` 原来只挡 `/` 与家目录本身，`ignored = ["/home"]` 照样放行整个主目录。
    改为"家目录本身及其任何祖先一律拒绝"，比较换成词法+真实两形态（HOME 是软链时连 `~` 都挡不住）。
    顺带修 `relUnder("/", "/etc")` 恒 false（拼出 `//`）的老坑——它让"拒绝 `/`"只能靠调用方记得补一条。
  - §6.5 的 `paths` 口径自相矛盾（一边说"在 upper 里算通过"，一边说"bind 被盖掉会落进 upper"）。
    统一成：**本该被 bind 走的四个来源**（公共目录 / AC_BASE / shared_writes / paths）出现在 upper
    就是 bind 失效 → `extract_failed`（退出 4）；通过档只剩 `ignored` 与 `unsupported`。
  - 摘要把 `leak` 强制读成 retained 会让 CLI 断言"证据已保留"，与"文件可能已被删"相反 → 照实读；
    `reason` 过枚举校验；重放时优先用摘要里的 `ignored`（审计当时的名单）。
  - **补回上轮重构误删的 `TestVerifyEvidenceCompleteness`**（五条证据情形），这是证据链回退，
    复审抓出来的——记一笔：删测试比写错代码更隐蔽。
- 2026-11-07 **第十五轮：把上一轮"记录不修"的项清掉**（复审被 429 挡住后接着做）：
  - **软链 XDG 根**（原 P1）：`~/.config` 指到 `~/cfg` 时，upper 里是真实路径、档案里是词法路径，
    只比一种就"认不出来"，候选凭空消失。新增 `looseRel`（词法+真实各试一次），分类与归并全部改用它；
    断言覆盖两种形态与"归并后候选不丢"。
  - **summary.toml 的形状**（原 P2）：`ignored` 以前是 `toml:"-"`（永远写不进摘要，违反"必须列出
    被忽略的类别"）；`conclusion` 不校验枚举（手写 `"OK"` 会让退出码 0 而 CLI 打印 unverified）；
    `leak-detected` + `evidence_retained = false` 会被照抄成"没保留"。三处都收紧了，并补 `generated_at`。
  - **`ProfileFromCandidates` 跨字段去重** + `unsupported` 补 `reason`：同一路径不能既进 `paths`
    又进 `shared_writes`（否则 bwrap 先隔离再共享，结果取决于参数顺序）。
  - **doctor 三处"假通过/越权"**：`state-dir` 不再 `MkdirAll`（它宣称只读），改为向上找第一个存在的祖先
    判可写；`/dev/fuse` 用 `Access(R_OK|W_OK)`（`Stat` 分不清"存在但打不开"）；userns 两个 sysctl
    都读不到时给 `warn`（原来判 ok）。
  - **`libVersion` 只对 GTK 解码**：`libadwaita-1.so.0.5.0` 套 GTK 的 `minor*100` 公式会读成 1.15.0
    —— 宁可"无法判定"，也不能给一个看起来很像的错版本。
  - 文档补两处已知取舍（`~/.local/state` 同时出现在 ignored 与可勾选根里不矛盾；verify 的
    `run.lock` 只在锁文件已存在时才 `LOCK_NB`，因为它是只读命令）。
- 2026-11-07 **第十四轮：实现审查判 REJECTED 后的修复**（1 P0 + 8 P1，其余记入待办）：
  - **P0 公共目录清单一直在退化**：`user-dirs.dirs` 的键是 `XDG_DOWNLOAD_DIR`，解析只剥了 `_DIR`
    留下 `XDG_DOWNLOAD`，而查表用 `DOWNLOAD` → 永远查不到，`PublicDirs()` 一直只返回英文默认名。
    中文系统的 `~/下载` 因此既不被判公共目录、也不挡 `paths`（第一原则的判据来源失效）。
    已修 + 断言（临时 `user-dirs.dirs` 写 `$HOME/下载` 后必须命中）。
  - **硬排除被归并抹掉**（P1）：`bookmarks` 是文件级 shared，归并成 `~/.config/gtk-3.0` 后又按
    config 勾选 → 集成隔离、书签消失。改为"目录内的条目是 shared 就把目录升级成 shared"，
    并把 `gtk-3.0`/`gtk-4.0`/`fcitx5`/`ibus` 直接列进硬排除表。
  - **单体文件进 paths 会炸**（P1）：`~/.config/mimeapps.list`、`~/.apprc` 在实例里被 `MkdirAll`
    建成目录，bwrap 就成了"目录 bind 到文件目标"，整条命令失败。改为按宿主实际形态建文件占位。
  - **XDG 根的祖先成了候选**（P1）：`~/.local`、`~/.var` 只因下级有写入而出现在 upper，
    勾中 `~/.var` 会隔离所有 Flatpak 数据。已在 `ownerOf` 里剔除。
  - **verify 的假通过**（P1 ×3）：`ignored`/`shared_writes` 从不校验，写 `~` 进去等于全部放行
    → 新增 `err.profile.blanket_path`；`paths`/`unsupported` 漏出通过档；摘出失效（公共目录 /
    AC_BASE / shared_writes 出现在 upper）过去被当成"预期共享"→ 改为 `extract_failed`（退出 4、保留现场）。
  - 其余：`.wh.` whiteout 去掉前缀再对账；`ignored` 只补缺失不覆盖显式空数组；`RemoveLauncher`
    绕过删除白名单；白名单不再接受前缀本身；`rm` 改为先删入口再删实例（否则留下死图标，
    `repair-desktop` 修不了）；`doctor` 不再 `MkdirAll` 数据目录（它宣称只读）。
  - **仍未修（记在待办）**：软链 XDG 根时归并会退化成灰区、`summary.toml` 的 `ignored`/时间字段、
    `libVersion` 对 libadwaita 仍可能猜出版本、`Shared`/`State` 与 `DefaultIgnored` 的口径冲突、
    CLI 人类文案未 i18n（全仓既有问题）。
- 2026-11-07 **第十三轮：`doctor --verify` 从桩变成判定引擎**（缺的只剩证据生产者）：
  - `core/verify.go`：`Verify` 把 §6.5 表格的六种情形全实现——从没审计（`audit_not_enabled`）、
    upper 有而 `run.toml` 缺（`audit_incomplete`）、空 upper（`no_writes`，"空集不算通过"）、
    只剩 `summary.toml`（重放已存结论）、摘要损坏（`audit_unreadable` + 保留）、正常对账。
    顺手补上原先漏掉的 `run.lock` 守卫（运行中读到的 upper 是半截证据 → 退出 7）。
  - `Judge` 是纯函数：不读盘、不删任何东西。"判定"与"删不删证据"分开是刻意的——
    判定错会误导用户，删错不可逆。公共目录出现在 upper 里**压过 `shared_writes`** 判泄漏
    （审计层把这些目录 bind 回真实位置，它出现在 upper 就说明摘出失效，那是第一红线）；
    被顺带造出的父目录（`.config` 只因 `.config/X/y` 存在）不单独报，否则真凶被淹没。
  - **本版本 verify 只读**：不写 `summary.toml`、不删 upper、不做 `quarantine-*`。没有生产者时，
    任何"删除证据"的动作只可能删掉别人放进去的东西。
  - 冒烟：手工构造实例后 `doctor --verify` → `unverified` + `audit_not_enabled`，退出 0，
    `--json` 的 `ignored` 是默认噪声清单（不再输出 `null`）；实例不存在 → 5。
- 2026-11-07 **第十二轮：L2 语义分类与 upper 归并落地**（探测管线里唯一能离线验证的一半）：
  - `core/classify.go`：§5 有序规则表，先命中先定。三条硬优先级各有断言——公共目录（含其下任意路径）
    压过一切、共享可写压过 `~/.config/*` 默认勾选、凭证进 `unsupported` 而不是 `shared_writes`。
  - `core/upper.go`：`MergeUpper` 取"XDG 根下第一级 / `$HOME` 下第一级"，剔除 `~/.config` 这类纯父目录，
    whiteout 单列（"删过"不是"写过"，摆给用户勾会让人去隔离一个已经删掉的路径），输出按路径排序
    （不排序 GUI 分组会跳、golden 也无从写起）。`ProfileFromCandidates` 把候选翻成档案三落点。
  - **踩到的坑（值得记）**：`hasCacheComponent` 最初扫整条绝对路径，结果测试环境的 `$TMPDIR`
    恰好在 `~/.cache` 下，整棵临时树被判成缓存。改成只看主目录之内的相对段——这也正是真实
    用户 `~/.cache` 语义该有的范围。断言抓到了它，否则线上表现是"所有路径都默认不勾"。
  - **设计稿与实现的已知偏差（GUI 落地时必须改稿，不许照抄）**：稿子把 `~/.dingtalk` 这类顶层点目录
    画成 `config` 且默认勾选，core 按 §5 判为 `dotdir`/`unsure`（灰区不勾）。`discover.md` §5 早已
    写明那是演示简化，但这意味着**屏3 原生实现要新增一个 chip 与一组灰区**，不能照搬稿子的两组。
- 2026-11-07 **第十一轮：实现审查的剩余 P1/P2 收口**（3 P0 已在上一提交修完）：
  - **删除白名单**（P1）：`Env.Trash` 先断言目标确属 `instances/`、`profiles.d/`、`appclone-*.desktop`，
    越界报 5 且**不调用 `gio`**；`LauncherPath` 与白名单共用 `LauncherDir()`，避免两处 `filepath.Join` 漂移；
    目标已不存在改为静默跳过（并发两次删除不再报假故障）。5 个新单测钉住白名单/越界/缺失三种情形。
    **踩到的坑**：`IsUnder(parent, child)` 的参数序被我写反，测试立刻抓到——这类"看起来对"的调用只能靠断言。
  - **回滚链**（P2）：`os.MkdirAll` 的手下会顺手建出多级父目录却只把最内层交给你，回滚时那几级空目录就永远
    留在宿主上。新增 `mkdirAllTracked` + `removeCreated`（由外到内建、由内到外删），2 个单测覆盖"整链回收"
    与"已存在的祖先不算本次新建"。
  - **GTK 版本探测**（P1，机制受限）：`pkg-config` → 库文件名 libtool 版本段（GTK `4.24.1 → …2400.1`）
    → `warn` + "无法判定版本"，三档都把来源写进结论、都不冒充通过。libadwaita 多数发行版是平 soname，
    拿不到就明说拿不到。真正的 dlopen 探测要等 purego 依赖落地（现在 `proxy.golang.org` 502，拉不动）。
  - **口径落到文档**：`LoadProfile` 只做解析与版本判定，语义校验归 `ValidateProfile`（否则"路径失效的档案"
    连读出来给用户看都做不到）；`rm` 的可删范围升为 §4 的硬约束。
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

## 未完成的审查（明确记录，不是静默跳过）

- 2026-11-07 修复提交 `ab07b05` 的**第二轮只读复审未能执行**：subagent 调用被频率限制挡住
  （429，18:52 UTC+8 重置）。AGENTS.md 要求"subagent 不可用时明确记录阻塞"，故记在这里。
  已做的替代自查（可复核）：`grep` 确认 `unverifiedWhy` 覆盖 `core/verify.go` 里全部五个 reason 值
  （`audit_not_enabled`/`audit_incomplete`/`no_writes`/`audit_unreadable`/`extract_failed`）；
  `Expand` 对 `..` 直接报 2，所以 `~/..` 绕不过 `blanket_path`；`--purge-profile` 的目标是
  `profiles.d/` 的真子项，仍在删除白名单内；gofmt/vet/test 与 `node design/guitest.js`（58/58）全绿。
  **下次开工第一件事**：把 `ab07b05` 交给只读 subagent 复审一遍（重点：`extract_failed` 是否掩盖真泄漏、
  文件路径占位的竞态、shared 升级是否过度共享）。
