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
**第二十六轮：审计证据链对抗审查（REJECTED）修复 + S0 完成**（2026-11-07）。只读 subagent 对工作树判 REJECTED
（2 P1 + 10 P2）；P1 全部修完：① 残留清扫改为按实例判断并在清扫期间**持有**该实例的 `run.lock`
（判断与搬移之间不再有窗口；跳过已写完 `run.toml` 的证据，两个实例同时审计不会互相卸挂载）；② `run.toml` 增加 `binds_recorded` 与 `paths`，老凭证回退按档案推断
（不再把 extract_failed 判成 leak）。P2 修完：声明但当时不存在的 `shared_writes` 算通过、父级软链逃逸拒绝、
`extract_failed` 搬 quarantine 后摘要写失败带真实路径、`audit_incomplete` 输出最新 quarantine 路径、
删除旧凭证失败拦住新审计、应用已启动后异常退出保留写层、跨设备拷贝 fsync 目录项 + dst 已存在拒绝、
classify 精确路径改 loose 比较、老/坏凭证与 EXDEV 回退补齐断言。`gofmt`/`go vet`/`go test ./...` 全绿；
`design/probe-smoke.sh /tmp/appclone` 仍退出码 0。
**S0 spike 五条通过**：本机（GTK 4.24.1 / libadwaita 1.10.0）与 `ubuntu:24.04` 容器（GTK 4.14.5 / libadwaita 1.5.0，
Xvfb）同一份 `CGO_ENABLED=0` 程序均跑通 SwitchRow notify→Go 回调、NavigationView push/pop、AlertDialog response
分发、中文渲染；**Esc 会触发 NavigationView pop**，GUI 实现必须在窗口级拦截（§4.3 语义）。

**第二十七轮：审计证据链复审收口（APPROVED）**（2026-11-08）。对抗性复审在工作树上判 **APPROVED**
（0 P0/P1，2 P2 + 4 P3）；P2 两条已修并补断言：① 清扫时 `readUpperTree` 读失败不再 `RemoveAll`，
改为搬进 `/var/tmp` quarantine（chmod 000 子目录曾会整棵删掉可能含用户文件的证据）；
② 新审计开始时不再只是删 `run.toml`，而是写一条 `finished_at` 为空的**进行中凭证**——
新审计若在写层建立前失败，verify 判 `audit_incomplete`，不会把旧 `summary.toml` 重放成"本次通过"
（重放文案同时改成"上次审计"）。P3：`moveDir` 预检测试改用空 dst（去预检会真红）、
`SharedBinds` 补断言、`run.toml` 增 version 超前拒绝、fsmove 测试标注禁 `t.Parallel`；
`lockAuditWork` 清扫期瞬时 exit 7 与空 `paths` 回退保留为注释说明（前者是原子化代价，后者方向保守）。
证据：`gofmt -l .` 空、`go vet ./...` 干净、`go test ./...` 全绿（GVM go1.26.5），
`design/probe-smoke.sh /tmp/appclone` 退出码 0（9 步全过）。

**第二十八轮：GUI v1 三屏落地（`implemented`）**（2026-11-08）。`cmd/appclone-gui/` 以 puregotk 直连
GTK4 + libadwaita，零 cgo：列表（SwitchRow/运行态/搜索/分页/空态只取代一组）、详情（启动/重命名/删除、
只读档案、最近审计、重新探测）、首次配置四态机（probing→confirm→creating→error；确认页四组 +
「共享但可写」代价文案；strace 降级横幅常驻）。Esc 在窗口级拦截（`NavigationView` 默认会 pop），
返回时取消探测并回弹开关；`core` 仍是唯一事实来源（GUI 只调 `ListApps`/`CreateInstance`/`Probe` 等）。
**冒烟暴露并修掉一个真实缺陷**：首屏全量扫描与 2 秒运行态轮询共用 `reloadNo`，首轮扫描超过两秒时
轮询会把尚未回写的结果判成过期 → 首次启动可长时间空列表；改为两个独立代数 + `loaded` 门。
证据：`gofmt -l .` 空、`go vet ./...` 干净、`go test -count=1 ./...` 全绿（新增
`TestProbeReportsFallbackToGUI`）、`CGO_ENABLED=0 go build ./...` 通过、`node design/guitest.js` 58/58；
broadway + chromium/puppeteer（假 HOME）截图确认列表/搜索/详情/删除确认默认「取消」/返回/open 确认页，
并从 GUI 建出实例（`instance.toml` 落盘）。**未像素验证**：probe→confirm→create 的页面切换——
`gtk4-broadwayd` 在 push 后频繁以 `broadway-output.c:458 append_node_ops` 断言自崩（上游缺陷），
应用进程与状态机回调均正常（instrumented 构建确认 Probe 返回、result idle 执行），该链路 core 部分由
CLI `add` 真机冒烟覆盖。GTK 侧回归闸门仍未建立，故 STATUS 只标 `implemented`。

**第二十九轮：GUI 实现审查（REJECTED）修复**（2026-11-08）。只读 subagent 判 **REJECTED**
（1 P0 + 7 P1 + 11 P2 + 11 P3）。**P0 是 purego 的 2000 个 C 回调槽**：旧实现每次渲染给每个控件
新建闭包变量再 `Connect`，而搜索每敲一个字就重建一次列表 —— 约 40 次重建后
`purego: the maximum number of callbacks has been reached` 直接崩进程。改为**五个稳定回调**
（notify/clicked/activated/response/idle，`callbacks` 字段，地址恒定、注册一次后只读）+
按 GObject 指针索引的处理器表 + `renderScope` 重建时整表注销；新增控件一律走「登记即连接」的
三个方法（第一版只把处理器写进表、忘了 Connect，冒烟时表现为「按钮点了没反应」，已修）。
P1：`hold` 加锁（后台线程不再 append 切片）；creating 期间 `SetCanPop(false)` 且 Esc 无效；
创建成功后落到**详情页**（§6.1；收尾放进 `popped` 再 push，不和 pop 动画打架）；关闭开关删除前
**重判运行态、逐个收集错误**（不再在部分失败时弹「已删除全部分身」）；rename/remove 的后台结果只在
仍是当前详情页时改界面；`buildProfile` 补回 `Env`，`ignored = []` 不再被默认表覆盖。
P2：setup/reconfig/rediscover **一律探测**（内置模板只作预填，分类仍走 `Classify`），确认按钮门禁
只数「应用自身数据」（只勾缓存/单例仍禁用），搜索 120ms 防抖，轮询 1s（详情）/2s（列表）分层 +
窗口不可见时暂停，Esc 在探测/确认页等于取消，app id 改 `io.github.mensylisir.Appclone`，二次启动
present 已有窗口（不再建第二个窗口）。core：`fallback` 进度事件移到「应用没在跑」闸门之后。
**未修并记录理由**：① GUI 文案本地化（ADR-027：`u.lang` 固定 `zh_CN`，静态文案暂留 GUI 代码，
避免中文界面插英文 reason；退出条件=GTK 闸门落地后按 `ui.*` 入表并改回 `i18n.Lang()`）；② 列表页致命/降级横幅、GTK 侧回归闸门
（见"被挡住的"）；③ `CachedAuditVerdict` 仍不能区分"没有审计证据"与"证据读不出来"，GUI 文案
保持保守说法（要改就得动 core 签名，收益低于风险）。
证据：`gofmt -l .` 空、`go vet ./...` 干净、`go test -count=1 ./...` 全绿、`CGO_ENABLED=0 go build ./...` 通过、
`node design/guitest.js` 58/58；broadway 冒烟（假 HOME → probe → confirm → 创建 → 详情页落盘
`instances/telegram/main`）与**重建 hammer**：搜索击键触发 150 次 `renderList`，进程存活、无 panic
（旧实现约 40 次即崩）。

**第三十轮：GUI 复审（REJECTED）与档案合并收口**（2026-11-08）。第二次只读 subagent 判 **REJECTED**
（3 P1 + 8 P2 + 7 P3）。
P1：① **重新探测会抹掉用户配置**——GUI 用内置模板/零值重建档案，CLI 的 `add` 在模板与探测两条路上也有同样问题；
抽出 `core.MergeNonPathFields`（重新配置/重新探测**只重定 `paths`**，`env`/`extra_binds`/`isolate_dbus`/
`isolate_runtime`/`sockets`/`cwd`/`audit`/`ignored`/未知字段以既有档案为准，探测给出 `exec` 时以探测为准），
GUI 与 CLI 共用一份规则；② 内置模板的非路径字段过去只被用来"预填路径"，`isolate_*`/`env` 等全丢，
确认页也没有 runtime 开关、详情页不显示 runtime 状态——改为 `applyBaseFields` 按"既有档案 > 模板"合并、
确认页按需出现「隔离私有 runtime 目录」开关、详情页单例行补「私有 runtime 已启用 / runtime 共享」；
③ 探测 goroutine 在 `Probe` 的 defer 清理完成前进程可能退出（GUI 无 daemon，窗口关掉就走）——改为
`Application.Hold()/Release()`（幂等）包住探测、保存档案与创建实例，清理一定跑完。
P2：零写入判定改用 `ProfileFromCandidates` 的派生结果（模板预填的非空 `paths` 不再掩盖"本次零写入"）；
bwrap 缺失/全局锁被占/应用在跑/零写入四类错误各有标题与重试路径，启动即异步 `CheckBwrap()`（新增
`core/envcheck.go` + i18n 文案），bwrap 缺失弹致命横幅并禁用开关/搜索，fuse 缺失只是可「了解」的降级横幅；
保存档案与创建实例拆成两段可重试事务（失败不落半成品）；重命名持全局写锁（ADR-022）；关闭开关的整批删除
先数运行态再删、按 deleted/running/failed 如实汇总；详情页档案读不出时显示原因页；模板-only 的档案
`probe_method` 标 `manual`；新建控件全部登记 `Unref()`（puregotk 构造器 `g_object_ref_sink`、无 finalizer），
页面重建与退出统一释放。
P3 七条全部落地：回调缓存注释订正（地址恒定 ≠ 可以重建）、`CachedAuditVerdict` 返回 `reason` 让详情页区分
四种 `unverified`（推翻第二十九轮 ③ 的「收益低于风险」判断：evidence 读不出被显示成「没结论」会把用户引向重跑而不是修文件）、搜索"结果太多"提示行不可激活、空态改用 `AdwStatusPage`、错误态标题按原因映射、
重建顺序固定为「先摘行 → 注销处理器 → Unref」、软横幅「了解」回调随 scope 释放。
**顺带修掉一个 schema 保真缺陷**：`ignored` 线格式用 `[]string + omitempty` 会把显式的 `ignored = []`
（用户关掉默认噪声白名单）在保存时抹掉，重新加载变回 nil 再被默认表替换；改为指针字段区分"没写"与"空表"，
新增 3 条 save→load 往返断言，并把规则写进 `方案设计.md` §5.5。
证据：`gofmt -l .` 空、`go vet ./...` 干净、`go test -count=1 ./...` 全绿（新增 `core/envcheck_test.go`
（3）、`core/merge_test.go`（3）、`core/profile_test.go`（3））、`CGO_ENABLED=0 go build ./...` 通过、
`node design/guitest.js` 58/58；broadway + 假 HOME 实测：致命横幅禁用/恢复、160 次 `renderList` 重建存活、
rename 持锁、删除进回收站、档案坏掉显示原因页、probe→confirm→创建→详情页落盘、`add --rediscover --profile-only`
保留非路径字段并更新路径。

**第三十一轮：GUI 复审第二轮（REJECTED）修复**（2026-11-08）。只读 subagent 对第三十轮工作树再判
**REJECTED**（2 P1 + 3 P2 + 5 P3）。
P1：① **列表行跨异步边界 use-after-free**——开关流程（首次配置要跑几十秒、关闭确认框可开数分钟）捕获了
行指针，期间 2 秒轮询/环境自检重建列表会 Unref 旧行；失败回弹时 `row.SetActive` 落在已释放对象上。
改为 `rowRef` 绑定渲染代数（`ui.listGen`），代数变了就只依赖重建后的真实状态，绝不碰旧控件。
② **Hold/Release 只包住探测**——保存档案/创建实例期间关窗会直接结束进程，`CreateInstance` 的回滚链跑不到，
留下"实例目录在、档案未提交"的半成品；`saveProfile` 还在 goroutine 里读 `v.done`（数据竞争）。
改为 `hold()/release()` 覆盖探测与两段事务，creating 态**拒绝关窗**（与 Esc/返回同一条规则），
并删掉 goroutine 里的 `v.done` 读取（事务一旦开始就跑完）。
P2：① **CLI 有内置模板就完全不探测**（`--rediscover` 也不探测）——与 §4/§6「模板只作预填」和 GUI 的
"一律真探测"直接冲突。`add` 改为始终 `Probe`，模板路径经新增的 `core.PrefillCandidates` 补进候选
（分类仍走 `Classify`），`paths` 为空按派生结果判定、纯模板预填标 `manual`；GUI 的预填也改用同一份实现。
② **version > 1 的档案被静默覆盖**：CLI `add` 吞掉 `LoadProfile` 错误继续探测，GUI `start()` 把
version_ahead 当"没有档案"。两端都改为拒绝加载并停止（GUI 显示"档案由更新版本生成"且不给重试），
新增 CLI 断言证明档案一个字节都没被改写。
③ **`start()` 的既有档案错误处理**与上面同一处修复；错误态没有重试路径时不再显示点不动的「重试」。
P3：① 导航页每次 push 泄漏整页（构造器 ref_sink、popped 后从未 Unref）——`releasePage` 在 popped 后
延后一个 idle 释放，`design/navspike` 自驱动 40 轮 push/pop 退出码 0（证明该时机安全）；
② `candidatesFromProfile` 把所有 unsupported 硬映射成 keyring（会改写白名单语义）——只对 `kind=keyring`
生成候选，其余原样经合并保留；`probeUnsupported` 改用 store 形态，合并去重按 resolver 归一
（`~/x` == `/home/u/x`）；③ 部分失败提示漏报运行中实例数——`deleted+failed+running` 必须对得上总数；
④ GUI `buildProfile` 重复实现合并规则——改为统一调用 `core.MergeNonPathFieldsWith`，界面开关只覆盖
`lock_files/isolate_*`；⑤ `lock_files` 冲突时改为**既有档案优先**（与函数注释和 §5.5 一致），
只有 base 为空才用探测值。
证据：`gofmt -l .` 空、`go vet ./...` 干净、`go test -count=1 ./...` 全绿（新增 `core/prefill_test.go` 2 条、
`core/merge_test.go` 2 条、`cmd/appclone/add_test.go` 1 条）、`CGO_ENABLED=0 go build ./...` 通过、
`node design/guitest.js` 58/58、`design/navspike`（`GDK_BACKEND=x11 DISPLAY=:0`）40 轮 push/pop 退出码 0、
`design/probe-smoke.sh /tmp/appclone-cli-new` 退出码 0（改后的 `add` 走真探测 → 档案 → 实例 → 审计 → verify → 回收站全链路）。
**本轮未能交互验证 GUI 三屏**：broadwayd 默认监听抽象 socket、GDK 客户端却找文件系统路径，
`-u` 又会让 HTTP 端口消失，socat 桥接后协议报 `Unknown input command`；本会话的 X 会话禁止 XTEST
注入（xdotool 指针/键盘无效）。渲染生命周期的新假设由 navspike 覆盖；三屏交互证据仍以第二十八～三十轮
的 broadway 记录为准。

**第三十二轮：第三十一轮增量复审 5 条 P3 收口**（2026-11-08）。增量复审判 **APPROVED**
（0 P0/P1/P2），5 条 P3 全部修完再提交：
① 两处注释仍说"导航页交给 nav 不释放"，与 `releasePage` 相反——改为"popped 后延后一个 idle Unref"，
并注明 navspike 证据；② 开启/新建分身（复用档案）失败时仍把 `retry` 指回 `start()`——version_ahead/损坏
档案没有可重试的动作，会得到一个永远失败的重试按钮。抽出 `showProfileRejected`，四个入口共用，
§6.1 ③-d 补"档案拒绝加载"这一例外（「重试」按有没有可重试动作显隐）；③ GUI 复用档案时凭空补
`DefaultIgnored()`，与 CLI 原样落盘写出不同字节——删掉这处（及探测后同一条死代码），`ignored = nil`
的"用默认噪声表"语义由 `verify.go` 兜底；④ 删除事务没有 `Hold`，关窗会在 `gio trash` 中途带走进程——
新增 `ui.beginBusy/endBusy`（主线程 `Hold/Release`），`confirmDisable` 与 `detail.remove` 两处包住，
`busy > 0` 时关窗被拒并提示；⑤ `detail.remove` 删完最后一个实例无条件 `nav.Pop()`——用户已返回时
会连带弹掉列表页。改为 `popIfVisible`（`GetVisiblePage` 比对后 `Unref`，非可见页交给 popped 收尾）。
随后对这些修复再跑一次只读增量复审：**APPROVED**（0 P0–P2），抓出 1 条漏修并已在本轮修掉——
⑥ ③ 的修复其实是 no-op：`ProfileFromCandidates`（`core/upper.go`）本身会给 `ignored` 填默认噪声表，
而 `MergeNonPathFieldsWith` 在基线为 nil 时不覆盖，所以"nil 透传"的注释并不成立。改为 merge 之后
`p.Ignored = slices.Clone(eff.Ignored)`——复用档案保持 nil、显式 `ignored = []` 保持空切片、
探测流程仍是 probe 给的默认表（与 CLI `--rediscover` 同口径）。
证据：`gofmt -l .` 空、`go vet ./...` 干净、`go test -count=1 ./...` 全绿、`go test -race ./core ./cmd/appclone`
全绿、`CGO_ENABLED=0 go build ./...` 通过、`node design/guitest.js` 58/58 退出码 0。
GUI 交互仍受本轮环境限制（见"被挡住的"），上述生命周期结论以代码纪律 + navspike 为准，不冒充像素验证。

**第三十三轮：GUI/CLI 探测落案编排去重**（2026-11-08）。此前"模板预填 → 来源标注
（manual/overlay）→ 零写入判定 → 非路径字段合并"在 CLI `add` 与 GUI `applyProbeResult`
各有一份实现——规则本身（`PrefillCandidates` / `MergeNonPathFieldsWith`）只在 core，
但**顺序与判据**（用哪个路径数判零写入、什么时候改标 manual、既有档案与模板谁优先）
两边靠人工同步，是最后一个会分叉的入口（STATUS 原「未落地 ②」）。
新增 `core.FinalizeProbe` 把这四步收成一个函数，CLI 与 GUI 只传各自的基线
（`Bundled` / `Existing`）与拿回 `(profile, candidates, prefilled)`；GUI 的
`mergeBundledPrefill` / `applyBaseFields` 两个方法与 CLI 的那段内联逻辑一并删除。
顺带把 GUI 的坏模板从"静默忽略"改成显式失败态（内置模板是编译期常量，读不出来
说明构建坏了，不给「重试」）。
证据：新增 `core/finalize_test.go` 5 条（零写入拒绝且 `observed` 含预填候选、纯模板预填标 manual、
本机探测不被预填改来源、既有档案 > 模板、无既有档案时模板作基线）；`gofmt -l .` 空、`go vet ./...` 干净、
`go test -count=1 ./...` 全绿、`go test -race ./core ./cmd/appclone` 全绿、
`CGO_ENABLED=0 go build ./...` 通过、`node design/guitest.js` 58/58、
`design/probe-smoke.sh /tmp/appclone-cli-b622999` 退出码 0（重构后的 `add` 真机跑通
探测 → 档案 → 实例 → 审计 → verify → 回收站）。
只读复审：**APPROVED**（0 P0–P2，4 条 P3）。已修 3 条：① 零写入用例补上"`observed` 含预填候选"
（这是本轮最易回归的语义，原用例无预填会假通过）；② 「内置模板读不出来」改用专用描述
（不再复用"请重新探测生成"——那正是该状态解决不了的动作）；③ `FinalizeProbe` 注释写明
"`res.Profile` 必须由同一 `res.Candidates` 派生、零写入时返回值不可用"的不变量；
STATUS 里的函数名 `MergeNonPathFields` 顺手改准为 `MergeNonPathFieldsWith`。
未改的 P3：`append(cands, extra...)` 的理论别名问题——真实 `MergeUpper` 产物 `cap==len`，
无调用方持有同底层数组的长 slice，改动（`slices.Concat`）只会增加噪音。

**第三十四轮：GTK 侧回归闸门落地（覆盖面受限）**（2026-11-08）。此前 GUI 只有 broadway 人工截图，
STATUS 因此停在 `implemented`。本轮新增 `cmd/appclone-gui/screens_test.go` + `design/guitest-gtk.sh`：
在**真实 GTK/libadwaita** 里用假 HOME 走一遍"列表空态可见 → 搜索过滤 → 拨开关 → 确认页路径行 →
创建实例 → 详情页（数据核对）→ 返回并清空 `activeDetail`"，任一断言失败或 45 秒不推进即以非 0 退出。
最终树连续 2 次 PASS、单次约 1.4–1.5s（`DISPLAY=:0`）。STATUS 记为 `verified（范围受限）`——覆盖的只是这条
页面生命周期，不是 GUI 全部交互。
**入口与用户操作的等价性要分层说**：开关这一环是真的——`setBool(row,"active",true)` 走 gobject
属性接口，与 `gtk_switch_set_active` 同路，会发真实 `notify::active` 进 `u.onSwitch`；确认页的
`v.submit()` 与按钮 clicked 绑的是同一个处理函数，属于直调处理函数而非合成点击。而本机 XTEST 被禁、
broadway 在 push 后自崩，所以这是目前唯一能自动跑起来的 GTK 闸门。测试文件带
`//go:build linux && gtk_e2e`：puregotk 在包 init 期 dlopen GTK，没有 GTK 运行库的机器来不及
`t.Skip` 就会 panic，默认 `go test ./...` 因此根本不编译它。
**审查后修掉的一个真实健壮性问题**：`AdwSwitchRow` 会在开关动画结束时（约 200ms 后）**补发一次
`notify::active`**。先前在测试里绕开（改从 `u.beginEnable` 进入），复审指出这是把产品缺陷当测试假象；
现在产品侧加结构性去重 `ui.toggled map[toggleMark]bool`（键 = 渲染代数 + 应用 key，值 = 已受理的
目标值），闸门恢复真拨开关；step 2 检测"原来那一页之外又冒出配置页"，step 3 保留 400ms
观察窗、step 4 返回后再复查一次。**变异测试证明有牙**：注释掉去重守卫后闸门在第 2 步
立即变红（不是等 45 秒超时），恢复后 PASS。
**复审（REJECTED → 修复）收口的其余项**：`GApplicationNonUnique` + 断言 `activated`/步数，堵住
"activate 被转发给别的实例、断言一次都不执行"的假通过；补 `AC_HOME` 并与 `HOME` 一起钉死
（`PathResolver` 用 `AC_HOME` 作可见 Home、用 `$HOME` 作 RealHome；只设 AC_HOME 不设 HOME 时
RealHome 仍指真实主目录，bwrap 的受限目录会跟着指错）；`AppDirList` 只扫闸门自己写的 demo/alpha 两个
`.desktop`，不再随本机 269 个应用漂移，且搜索过滤断言因此有牙；确认行验 `GetParent() != nil`，
堵住"忘了 Add 的用户空页"；`Mounter` 注入 stub，不在 `$TMPDIR` 触发真实 fuse 冒烟；脚本用
`CGO_ENABLED=0` 构建 CLI 与测试、支持 `--keep` 保留现场；失败时先报 `runErr` 再报 `activated/step`。
标准复审（第三轮）另给 9 条 P3，已逐条收口：脚本预检宿主 `bwrap`（缺则 exit 4，不再等 45 秒超时）、
`--keep` 真正保留假 HOME（`APPCLONE_GTK_ROOT` 放在 `$WORK` 下，测试不再用会被自动删除的
`t.TempDir`）、`onSwitch` 补旧渲染代数守卫、step 4 返回后再等 300ms 并断言不再冒出配置页、
注释与文档措辞按代码实际（`AC_HOME` 作可见 Home / `$HOME` 作 RealHome；确认是"直调同一个回调"
而非合成点击）；GTK 闸门写进 AGENTS.md 的完成定义（改 `cmd/appclone-gui` 必须跑）。
对抗性复审又抓到一个闸门完整性缺口并已修：`go test -run` 匹配不到测试时仍返回 0（测试被
移走/改名会静默假绿），脚本现在把输出落盘并 `grep '--- PASS: TestGtkScreensLifecycle'`，
没有这条就 exit 5；同轮把 step 2 的"又冒出配置页"改成按页面指针立即判红，不再靠 45 秒超时。
两条闸门完整性修复也做了反证：把 `screens_test.go` 移走后脚本 exit 5（不再 `[no test files]` 假绿）；
用只有 go/CLI 的最小 PATH 运行时脚本 exit 4 并直说缺 bwrap（不再 45 秒超时）。
**当时仍未覆盖**：探测四态、删除/重命名/关闭开关对话框、横幅与错误态文案、fuse 致命态回退、
指针与键盘事件注入——这些需要输入注入或可编程对话框响应，留给后续；ADR-027 的 i18n 退出条件
因此在 `docs/tech-stack.md` 里明确为"闸门走遍三屏四态之后"，`u.lang` 暂不改回 `i18n.Lang()`。
**第三十七轮已收口其中四项**（探测中/失败态、降级横幅与「了解」、fuse 致命态、三屏文案入 i18n），
只剩删除/重命名对话框与指针/键盘注入未自动化。
证据：`design/guitest-gtk.sh` 最终树连续 2 次 PASS（1.40s / 1.45s；含 `go vet`/`go vet -tags gtk_e2e`/`gofmt` 干净、
`go test -count=1 ./...` 全绿且 GUI 包默认不编译、`CGO_ENABLED=0 go build ./...`、
`node design/guitest.js` 58/58）。

**第三十五轮：界面视觉收口（用户反馈"太朴素、不如原型图"）**（2026-11-08）。仍是 libadwaita
原生控件、不写自定义 CSS（ADR-013/§4.6），只把稿子里承载信息的视觉元素补进原生结构：
- **列表行加应用图标**（`GtkImage`，32px；`Icon` 支持主题名与绝对路径，取不到退
  `application-x-executable`）。这是原实现最明显的缺口——30 行纯文字长得一模一样。
- **修掉空态被裁成半截图标**：`AdwPreferencesPage` 的 `Add` 只接受 `AdwPreferencesGroup`，
  而 Group 只接 `AdwPreferencesRow`；把 `AdwStatusPage` 塞进 Group 会得到一张只剩灰色半圆的
  空页。改为挂在搜索栏与列表页之间，`renderList` 只切 visible；空态时列表只剩「所有应用」组。
- **标题栏与搜索分层**：HeaderBar 恢复显示「应用分身」，搜索条单独一行、铺满宽度且不抢初始
  焦点（窗口映射后把焦点交给列表页），不再是顶栏里孤零零一个输入框。
- **详情页**：HeaderBar 换成 `AdwWindowTitle`（应用名 +「N 个分身 · M 个运行中」两行）；
  实例行加 `edit-copy-symbolic` 前缀，新建分身行改 `list-add-symbolic` + 设计稿副标题；
  启动/重命名/删除改用 `AdwButtonContent` 图标+文字；实例组/隔离设置组补 `SetDescription`；
  隔离路径、探测方式、单例处理、最近审计、实例根目录、重新探测各加语义图标（16px）；
  档案损坏态同样改为带警告图标的只读组（StatusPage 误用的另一个落点）。
- **探测页**：探测中显示 64px 应用图标（"在等谁"一眼可见）；确认页路径行加 `folder-symbolic`。
验证：`DISPLAY=:0` 真机截图核对了空态、有实例列表、详情页、确认页四张；Go 门禁全绿；
`design/guitest-gtk.sh` 2/2 PASS（空态断言已改为"StatusPage 可见 + 有 parent + 只剩 1 组"）；
`node design/guitest.js` 58/58（HTML 基线未动）。
对抗性复审的 P2 产品边界同时收口：open/clone 入口里档案在"列表扫描 → 用户拨开关"之间被删时，
不再落到只有「取消」的错误页，而是按 not_found 重新走首次配置（与 CLI `add` 同口径），
页面标题同步改成「首次配置 · X」。（临时在 /tmp 副本注入"拨开关前删档案"验证：闸门进入 probing/带「重试」的 error，而不是无重试终态，PASS。）
**未修的两条理论风险**（复审标记为未复现，均属既有逻辑）：删除事务仍是"先全量判运行态再逐个删"的
TOCTOU——删除循环期间用户若启动实例，可能出现部分执行（结果会如实报 deleted/running 计数）；
busy 事务没有超时，`gio trash` 或等锁卡死时关窗会被一直拒绝。留待后续评估，不在本轮改动。

**第三十六轮：列表拆成两个平级视图（用户反馈"上面一个滚动条、下面一个滚动条"）**（2026-11-08）。
用户指出「已开启」与「所有应用」上下叠在一页、一条滚动跨界很别扭。改法是原生的
`AdwViewStack` + 标题栏 `AdwViewSwitcher`（宽策略，图标+文字）：
- 两页各一个 `AdwPreferencesPage`，**各自滚动**；`on` 页 = 已开启分身 + 底部「添加应用分身」
  （`list-add-symbolic`，切到 `all`），`all` 页 = 搜索条 + 全部应用。
- **首屏落点按状态**：已有实例 → `on`（多数人是来启动/管理的），一个都没有 → `all`
  （点亮开关是唯一能做的事）。只决定首屏一次；创建成功后切回 `on`，返回时刚建出的实例就在眼前。
- **角标**：`ViewStackPage.SetBadgeNumber` 给「分身」页挂数量。首版判断"只在非当前页
  显示"是**误读截图**（角标与 copy 图标重叠、低分辨率下看成了图标的一部分）——用放大
  截图复核 + 对抗性复审的 PyGObject 实测确认：**当前页也画**角标，选中页与未选中页都显示。
  HTML 基线与文档已按真实语义改口（不再按当前视图隐藏）。
- **空态搬家**：「分身」页的列表位置换 `AdwStatusPage`（`vexpand` 居中，语义从"取代一组"变成
  "取代这一视图的列表"），里面是 suggested-action「选择应用」切到 `all`；`all` 页与切换器
  **始终可达**——首启死路这条不变式没变，只是路变成了两条。
- 去掉 `all` 页组的重复标题（视图名已经说了"所有应用"），组说明保留。
交互基线同步：`design/appclone-gui.html` 加标题栏 `AdwViewSwitcher`（内联 SVG 图标 + 角标）、
`#viewClones`/`#viewAll` 两个 pane、`showView()`、空态「选择应用」与「添加应用分身」，
组件映射卡与状态行一并改写。`design/guitest.js` **58 项不变**：新增 `showView()` 帮手，
空态两条改为"空态显示 + 「选择应用」通向可见的应用列表"与"还原后回「分身」"，
首屏落点断言折进「行点击进详情」；搜索/横幅测试先切到 `all`，横幅用例并断言
**致命态下视图切换仍可用**（这是 `AdwViewSwitcher` 与 bwrap 状态无关的语义；事件层
若把它一起短路，标题栏上就是一个可见但点不动的控件）。
- 首屏扫描落地前不绘制视图栈（`SetVisible(false)`），且 **switcher 同时禁用**（扫描期间
  内容区是空的，允许用户先选再被落点覆盖是糟糕体验）；恢复显示与解禁都放在 `renderList`
  末尾，即使某轮结果被更新的一轮丢弃也不会永久空白。首轮落地后把焦点交给当前视图的
  `PreferencesPage`（不是 ViewStack——它没有滚动控制器，方向键/PageDown 滚不动列表）。
- 对抗复审收口的其他两条：`TestGtkEmptyStateMapped`（切到 on 后断言空态 `GetMapped`、
  父容器就是 `onBox`、`onPage` 不可见——只断 `GetVisible`/`GetParent` 是假绿）；
  `TestGtkLandsOnClonesView` 增加"首轮之后切走 → 再 reload → 视图不弹回"的观察窗口
  （删掉 `first` 守卫会红）。
GTK 闸门补结构断言：无实例时 `GetVisibleChildName()=="all"`、创建后 `=="on"`；
另加 `TestGtkLandsOnClonesView`（预置实例 → 首屏必须 `on`，且列表可见、空态不显示）与
`TestGtkEmptyStateMapped`；三条都做过变异验证（不恢复显示 / 落点每次执行 / 空态挂错页，
分别立即变红）。`guitest-gtk.sh` 的 `-run` 与 PASS 检查覆盖三条用例（匹配不到就退出 5）。
证据：`gofmt -l .` 空、`go vet ./...` 干净、`go vet -tags gtk_e2e` 干净、
`go test -count=1 ./...` 全绿、`CGO_ENABLED=0 go build ./...` 通过、`node design/guitest.js` **58/58**、
`design/guitest-gtk.sh` **3/3 PASS**；`DISPLAY=:0` 真机截图核对了「分身」（含运行实例、角标）
与「所有应用」两屏。

**第三十九轮：克隆起步 `add --fork`（ADR-029）+ GUI 默认窗口加宽**（2026-11-08）。
路线图第 6 项落地：分身可以从主实例现有数据「克隆起步」，不复制文件。
- 语义是**运行期 per-path overlay**，不是 add 期拷贝：`add --fork` 只写 `instance.toml` 的
  `fork = true`；每次 `run` 对归一封顶后的每个 `paths` 条目挂一层 `fuse-overlayfs`
  （lower=宿主真实目录，upper=`<inst>/root/<原路径>`，`work`/`merged` 在
  `<inst>/.fork/<sha256(clean abs)[:8]>/`），merged 只作 bwrap ⑤ 的 bind 源。
  `shared_writes` 仍排在 ⑤ 之后 bind 真实文件——共享语义未被克隆改写。
- 只对**存在、是目录、非空**的 lower 挂载；空/缺失路径退回普通实例目录。fuse 不可用、
  单个路径挂载失败、残留卸不掉都只降级该路径并写 stderr，不阻断启动（安全语义仍由
  `paths` 白名单保证，代价只是少继承数据）。
- **stale 挂载回收的坑**：上一轮被 SIGKILL 时 `.fork/<hash>/merged` 会留在 mountinfo。
  不能用 `mounted()`/`os.Stat` 判断——fuse daemon 已死时 stat 返回 `ENOTCONN`，
  会被判成"没挂载"，接着 `RemoveAll` 穿过挂载点遍历 lower，**删的是主实例真实数据**。
  改为只查 `/proc/self/mountinfo`，顺序固定「先卸后删」；卸载失败保留 `.fork/`，
  等占用进程退出后的下一轮再清。smoke 时这两点各踩中一次，均已回归。
- **同一个坑的第二种形态（带空格路径）**：mountinfo 把空格转义成 `\040`，原先
  用 `strings.Contains(" "+path+" ")` 比对，`AC_BASE`/`HOME` 含空格时永远判不中。
  `fork.go` 的 stale 判断会漏（随后 `RemoveAll` 穿过挂载点），而探测链路的
  `mounted()` 同缺陷更直接——挂载其实成功了，轮询 5 秒后误判失败并卸载。
  抽出 `mountInfoHas`：按行取第 5 字段、解码 `\040/\011/\012/\134` 后整串相等，
  `mounted()` 也改为 mountinfo-only；回归 `TestMountInfoHasEscapedPaths`。
  带空格真机冒烟：`/tmp/ac fork space.*` 下两次 run 正常（`SEEN=main` → `clone`），
  SIGKILL 制造 stale 后下一轮自动卸载并继承，无降级警告。
- `--profile-only --fork` 是用法错误（退出 2）：不建实例就没有 upper 可落地，
  静默忽略会让用户以为克隆已生效。
- **语义边界如实写进 §8.1**：lower 是实时数据不是时间点快照；overlay 不保证分身运行
  期间主实例同时修改同一份数据的可见性，因此两者**不要同时使用**同一份应用数据，
  需要冻结起点须先复制。分身写过（copy-up）之后只读自己的 upper。
- **GUI 默认窗口加宽到 880×720**（用户反馈"打开一个竖条条"）：`SetDefaultSize(880, 720)`，
  与设计稿 880px 宽对齐；GTK 生命周期用例加断言（默认尺寸 < 800×600 即红，变异回
  560×760 实测立即变红），防止以后改回窄条。
- 文档同步：`方案设计.md` §8.1 重写为已实现契约（布局/寿命/降级/残留/语义边界，
  `.tmp-work`/`.tmp-merged` 旧路径清除），§4 白名单与权限矩阵改 `.fork/`，
  `instance.toml` 示例补 `fork`，§6 `add` 行补 `--fork`；`tech-stack.md` 新增 ADR-029。
- 证据：`gofmt -l .` 空、`go vet ./...` 与 `go vet -tags gtk_e2e` 干净、
  `go test -count=1 ./...` 全绿（新增 `core/fork_test.go`、`TestBuildBwrapArgsForkSources`、
  `TestAddForkFlagPersists`、`TestMountInfoHasEscapedPaths`）、`go test -race ./core/... ./cmd/appclone` 绿、
  `CGO_ENABLED=0 go build ./...` 通过、`node design/guitest.js` **60/60**、
  `design/guitest-gtk.sh` **13/13 PASS**（退出码 0）；假 HOME 真机 fuse 冒烟：
  第一次 `SEEN=main` → 分身写 `clone`、宿主仍 `main` → 第二次 `SEEN=clone`，
  结束后 `.fork/` 删除、挂载数 0；SIGKILL 制造 stale 挂载后下一轮自动卸载并正常继承。
- **对抗复审 REJECTED 的修复（2026-11-08）**：主路径语义（shared_writes 顺序、
  `--fork`/`--profile-only` 组合、Fork 持久化）被确认无问题，但安全不变量在两条路径上
  有洞，均已修：
  ① **软链路径别名**：mountinfo 记录 d_path（软链已解析），调用方拿词法路径；只比一种
     形态会让挂载轮询超时、stale 漏判、`RemoveAll` 穿过挂载点。新增 `pathForms`
     （词法 + `realPathBestEffort`，挂载点已死时退到最近可解析祖先）。
  ② **`Mount` 报错但挂载点已生效**：原先无条件 `RemoveAll(wd)`；改为先查 mountinfo，
     挂着就把卸载交给 cleanup，卸不掉保留 `.fork/` 等下一轮。
  ③ **mountinfo 分词**：`strings.Fields` 按 Unicode 空白切分会把 U+3000/NBSP 当分隔符；
     改为 ASCII 单空格 `SplitN`，四种内核转义解码后整串比较。
  ④ **`upper` 落在 lower 内**（AC_BASE 位于档案路径之下）：跳过该路径并告警，避免
     overlay 递归；`resetForkBase` 逐条卸载并加 `mountUnder` 兜底，仍挂着就拒绝删除。
  ⑤ **死挂载判活**：`mounted()` 拆成清理用（mountinfo-only）与轮询用 `mountLive`
     （mountinfo + stat），fuse crash 后不再把死挂载当成功。
  ⑥ **删除实例**：先卸 `.fork` 残留，失败返回 `err.instance.fork_cleanup` 并拒绝删除。
  ⑦ **停止响应**：`forkMounts` 每路径前检查 ctx/stopping，不再 N×5 秒后才响应。
  回归补 6 条（软链、Unicode 空白、reset 拒绝删除、Mount 报错保留现场、upper 在 lower
  内跳过、删除遇残留挂载拒绝）；带空格与软链 `AC_BASE/HOME` 的真机冒烟均通过。
  **复审 APPROVED 后的 P3 硬化**：递归防护改成两种路径形态都比；`realPathBestEffort`
  只解析父目录、不再对 merged 本身 `EvalSymlinks`（fuse 卡死时不会阻塞）；cleanup 的
  `RemoveAll` 也加 `mountUnder` 兜底；`.fork` 被应用建成普通文件时按 ENOTDIR 放行删除；
  `doctor --json` 补 `"fork": true`。
  **记录不修**：32 位哈希碰撞、`instance.toml` 未知字段重写丢弃（既有行为）、fuse `-o`
  逗号/冒号限制（探测/审计同源）、GUI 无 fork 入口（下一轮）。
- **剩余**：GUI 还没有 `--fork` 入口（新建分身仍建空实例）。接线要先改
  `design/appclone-gui.html` 的确认页（加"继承现有数据"开关）+ `guitest.js` + GTK 用例，
  工作包比 core 大，留到下一轮；`STATUS` 已按"CLI 已验证、GUI 未覆盖"如实标注。

**第四十轮：GUI 克隆起步入口 + 无图标应用的首字母头像**（2026-11-08）。用户反馈"无图标的应用太丑"。
- **确认页新增「数据来源」组**：`AdwSwitchRow`「从现有数据克隆起步」，默认关，只在本入口
  真要建实例时出现（setup/open/clone）；`rediscover` 只更新档案、不建实例，开关必须隐藏——
  否则拨了看起来生效、实际什么都没发生。提交时走
  `core.CreateInstanceWith(..., CreateOptions{Fork: v.fork})`，与 CLI `--fork` 写同一个
  `instance.toml` 字段，两端默认一致（都不 fork）。文案（zh/en）说明"不复制文件；分身写过的
  数据只影响自己"，不把 overlay 细节丢给用户。
- **无图标应用改首字母头像**：`Icon` 是主题名时先过 `GtkIconTheme.HasIcon`，是绝对路径时
  必须真实存在；取不到改 `AdwAvatar`（首字符转大写、空名 `?`），不再退化到
  `application-x-executable`——通用图标看起来像加载失败，30 行列表全是它等于没有区分度。
  列表行 32px、探测中页 64px 共用同一判定；设计稿本来就是字母色块（`.appicon`/`.insticon`），
  现在 GTK 侧终于与基线一致。中文名取首字（`微信` → `微`）。
- **交互基线同步**：`design/appclone-gui.html` 确认页加「数据来源」勾选组（`data-fork`），
  创建出的实例带 `fork` 标记，toast 补"已从现有数据克隆起步"；rediscover 页面不得出现该组。
  `design/guitest.js` 58 → **62 项**（默认关/拨开、实例 fork 标记、rediscover 不显示）。
- **GTK 闸门**：新增 `TestGtkForkTogglePersists`（默认关 → `SetActive(true)` 经真实 notify
  接到 `v.fork` → 提交 → `instance.toml fork=true`；只做个装饰性开关会由断言立即判红）与
  `TestGtkAppIconFallback`（空 `Icon`/不存在的绝对路径不得判为可用；中文取首字、拉丁转大写、
  空名 `?`）。`design/guitest-gtk.sh` 13 → **16 条 PASS**（另加 `TestGtkForkDisabledWhenDegraded`：降级态下开关必须禁用）。
- 文档同步：`方案设计.md` §6.1 确认页补「数据来源」组与 rediscover 隐藏规则；
  `tech-stack.md` §4.5 行图标回退改为 `AdwAvatar` 首字母色块。
- **只读复审 REJECTED 后的修复**（无 P0/P1，5 条 P2 + 5 条 P3）：
  ① 创建 goroutine 直接读 `v.fork` 与开关动画结束补发的 notify 构成数据竞争——
     改为在主线程快照 `fork := v.fork` 再进 goroutine（`-race` 可复现）；
  ② 开关测试只建对象不断言上树——补 `GetParent()!=nil`；否则漏 `group.Add` 时
     用户看不到开关、`instance.toml` 却照样写 `fork=true`，测试仍绿；
  ③ 图标测试没调用 `appIcon`/没覆盖主题分支——`appIcon` 返回 `fallback bool`，
     测试在 `gtk.Init()` 后断言空 Icon、缺失路径、不存在的主题名都退回，存在文件
     不退，并直接断言 `iconImage(不存在的主题名)==false`（去掉 `HasIcon` 检查会红）；
  ④ 稿子注释仍写「四态与四组」、`tech-stack` §4.5 确认态映射漏了「数据来源」——
     两者同步为五组并写明默认关/rediscover 隐藏/fuse 缺失禁用；
  ⑤ 缺 fuse-overlayfs 时开关仍可拨（run 期会降级成空实例）——改为 `SetSensitive(false)`，
     降级横幅已有原因；rediscover 隐藏分支补 GTK 断言（同一确认页按 mode 重渲染）。
  P3 顺手收口：`DisplayGetDefault`/`IconThemeGetForDisplay` 包装的 `IncreaseRef` 用完
  `Unref`；§6.1 副标题与模式列表对齐实现（subtitle 用消息表原文、含 reconfig）；
  各处「60 项」计数改为以 `EXPECTED` 为准；**并加 `appDisplayName` 兜底**——`.desktop`
  的 `Name` 解不出来时行标题/头像用 desktop id，任何情况下都不会出现一条既无图标
  又无名字的空白行（用户反馈的空白行在本机 273 个 .desktop 里没有对应项，见下方说明）。
- 证据：`gofmt -l .` 空、`go vet ./...` 与 `go vet -tags gtk_e2e` 干净、
  `go test -count=1 ./...` 全绿、`go test -race ./core/... ./cmd/appclone` 绿、
  `CGO_ENABLED=0 go build ./...` 通过、`node design/guitest.js` **62/62**、
  `design/guitest-gtk.sh` **16/16 PASS**（约 4.6s，退出码 0）；
  另以 `CGO_ENABLED=1 -race -tags gtk_e2e` 单跑 Fork/图标两用例通过（数据竞争修复有实测）。

**第四十一轮：打包与 CI（`implemented`）+ staticcheck 归零**（2026-11-08）。目标从"本机能跑"推进到"发布可用"。
- **打包配置落地**：`packaging/nfpm.yaml` 一份配置出 deb/rpm/Arch 三格式；`depends` 按格式覆写
  （deb `libgtk-4-1 (>=4.14)` / rpm `gtk4 >= 4.14` / Arch `gtk4>=4.14`），`fuse-overlayfs` 走推荐/可选。
  **nfpm v2.41 不对 `contents.src` 做环境变量展开**，发布流程先把二进制落到 `dist/stage/` 再打包。
  包内落点、postinstall 三件事（AppArmor profile / 桌面数据库 / 图标缓存）与 `tech-stack.md` §6 一致。
- **postinstall 的 AppArmor 处理**：只在 `kernel.apparmor_restrict_unprivileged_userns=1` 且
  `/etc/apparmor.d/bwrap` 不存在时安装；不改全局 sysctl，不覆盖发行版将来的同名 profile。
  任一步失败都以 0 退出——安装事务不能失败，环境问题交给 `appclone doctor` 如实报告。
- **发布资产**：`README.md`（安装/使用/排障/开发门禁）、`.desktop`（`Name[zh_CN]=应用分身`）、
  hicolor SVG 图标、`bwrap.apparmor`。
- **CI（`.github/workflows/ci.yml`）**：Go 门禁 job 跑 gofmt / vet（含 `gtk_e2e` 标签）/ staticcheck /
  `go test` / `-race` / 零 cgo 构建与 amd64+arm64 交叉编译；HTML 闸门 job（Chrome for Testing +
  puppeteer-core 25.16.0）；GTK 闸门 job 在 `ubuntu-24.04` 与 `ubuntu-24.04-arm` 各跑一次
  `xvfb-run design/guitest-gtk.sh`——arm64 是"能编译 ≠ 跑过"的实机验证，不是可选项。
- **发版（`.github/workflows/release.yml`）**：tag `v*` 触发；固定 nfpm v2.41.0；矩阵按文档
  `{deb,rpm} × {amd64,arm64}` + `archlinux × amd64` = 5 包，另出两个架构化 `.tar.gz` 与 `checksums.txt`，
  由 `softprops/action-gh-release` 上传。Arch 包名/架构、deb `Architecture: arm64`、
  rpm `aarch64` 已在本地用 `VERSION=0.0.0-test` 全流程出包并解包核对。
- **staticcheck 归零**：修掉 6 条既有告警（两处结构体转换、`Env.lock` 死字段、`append` 循环、
  navspike 变量声明合并），`staticcheck ./...` 无输出，因此可作为 CI 阻塞门禁。
- 证据：`gofmt -l .` 空、`go vet ./...` 干净、`go test -count=1 ./...` 全绿、`staticcheck ./...` 零输出；
  本地模拟 release 步骤（两架构交叉编译 → 5 包 → 2 tar.gz → checksums）全部成功；
  两个 workflow YAML 解析通过。**未验证**：GitHub Actions 实跑（本机无法复现 hosted runner 环境），
  故打包能力只标 `implemented`，CI 首次绿之前不标 `verified`。
- **只读复审 REJECTED 后的修复**（1 P1 + 5 P2 + 10 P3）：
  ① **P1：CI 的 GTK job 缺 `strace`**——GTK 测试注入 `noFuseMounter`，探测只能走 strace；
     两个 job 的 apt 列表补 `strace` 与 `dbus`，否则 `TestGtkRapidRepush` 会因探测直接报错而变红。
  ② `.desktop` 按 §4.8 改为 `Name=应用分身`（另加 `Name[en]`）、`Categories=Settings;System;`，
     删掉可能与应用 id 不匹配的 `StartupWMClass`。
  ③ `postinstall` 的桌面数据库/图标缓存 helper 不在最小依赖里：加 `desktop-file-utils` 依赖，
     图标缓存先试 `gtk4-update-icon-cache` 再回退 `gtk-update-icon-cache`（都缺则 best-effort 跳过）；
     AppArmor profile 改临时文件 + rename，中断不再留半截文件。
  ④ `.tar.gz` 补进 `bwrap.apparmor`，README 排障路径同步（手工安装不再引用仓库内路径）。
  ⑤ 包与文档补上 strace 承诺：deb/rpm `Recommends: strace`，Arch 受 nfpm 限制写进 README；
     `doctor` 新增 `probeBackend`（fuse + `/dev/fuse` + strace 三输入）——两者都缺时报 **fail** 而不是
     warning，并新增 `TestProbeBackend` 钉住六种组合。
  ⑥ README 修正：静态链接只描述 CLI（GUI 经 dlopen）、安装命令改 `apt install ./` / `dnf install ./`、
     Arch 的 postinstall 一并说明、AUR 源码包改为"尚未提交"、Go 门禁补 staticcheck 与 `gtk_e2e` vet。
  ⑦ release 硬化：tag 只接受 `vX.Y.Z`（nfpm 会把 `1.0.0-rc1` 在 Arch `.PKGINFO` 写成 `1.0.0`，等于
     把预发布当正式版发出去）；发版前重跑 gofmt/vet/test；nfpm tarball 加 sha256 校验；
     GUI 补 `main.version` 并用 `GApplication.SetVersion` 暴露。
  **记录不修**：license 字段/`LICENSE` 是用户/法务决策（tech-stack 建议 GPL-3.0-or-later，未获确认前不写；
  **后续：2026-11-08 用户已定 GPL-3.0-or-later，见 ADR-030**）；
  `ubuntu-24.04-arm` hosted runner 是否在当前账号套餐内无法从本机验证，CI 保持必需 job 而不是静默跳过；
  ~~actions 仍钉 tag 未钉 SHA~~（2026-11-09 已钉 commit SHA）；卸载时清理 AppArmor profile 未做
  （删除 `/etc` 文件比留着更危险，交给发行版/用户）。
- **二次复审后的收口**（2 P2 + 6 P3）：
  ① **P2：补 GTK 闸门证据**——`design/guitest-gtk.sh` 在最终树重跑，16 条全部 PASS、退出码 0
     （本机首次因 /tmp 配额不足构建失败，清理自有临时产物后通过；不是测试失败）。
  ② **P2：release 复用整套 CI**——`ci.yml` 增加 `workflow_call`，`release.yml` 的 `verify` job
     直接调用它（Go/HTML/GTK/GTK-arm 全跑），`release` 再 `needs: verify`；tag 来自任意提交时
     也不能跳过 HTML/GTK 回归。
  ③ `doctor --strict` 口径修正：`fuse` 检查进入 `required`——只有 fuse 与 strace **都不可用**
     时该检查才是 fail，`--strict` 才会红；只缺 fuse（有 strace）仍是 warn。补 `TestRequiredIncludesProbeBackendFailure`。
  ④ 提示语按实际能力分档：区分"普通分身仍可运行"与"`--fork` 克隆起步会降级"；两者都缺时
     明确"装 strace 最稳妥，fuse-overlayfs 还需 /dev/fuse 可读写"。`TestProbeBackend` 补
     "二进制缺失但 /dev/fuse 存在"两种现实组合。
  ⑤ deb `Recommends` 加 `libgtk-4-bin`（图标缓存更新器在 Debian 系属于它）；`docs/tech-stack.md`
     同步说明缓存是 best-effort，不抬成硬依赖。
  ⑥ AppArmor 安装再硬化：临时文件改为 `/etc/apparmor.d/.appclone-bwrap.new`（AppArmor 不扫描隐藏名），
     `apparmor_parser` 失败时删除目标以便下次安装重试；`doctor` 在文件存在但找不到 parser 时给 warn。
  ⑦ README 修正"两者都缺不能新建"的过强表述：已有合法档案仍可建普通分身，只是不能首次探测新应用。
- **Ubuntu 24.04 容器实装验证**（2026-11-08）：用 podman 起 `ubuntu:24.04`，`apt install ./appclone_1.0.0_amd64.deb`
  成功；核对 `/usr/bin/{appclone,appclone-gui}`、`.desktop`、SVG、`bwrap.apparmor` 落点与权限；
  `desktop-file-validate` 退出 0；非 root `appclone version` → `1.0.0`；`doctor` 判定 GTK 4.14.5 /
  libadwaita 1.5.0 正常；装包后的 GUI 在 `xvfb-run` 下运行到 8 秒超时（124，非崩溃）。
  **抓到并修掉一个真实缺依赖**：最小 Ubuntu 安装没有 `/usr/bin/gio`（它在 `libglib2.0-bin`，
  不在 `libglib2.0-0t64`），删除功能会因此不可用；deb 加 `libglib2.0-bin`、rpm/Arch 加 `glib2` 后
  重装复测 `doctor` 的 gio 项从 ✗ 变 ✓。容器内 `/dev/fuse` 打不开、无 D-Bus 会话属容器限制，
  不作为产品结论。
- **Fedora 41 容器实装**（2026-11-08）：`dnf install ./appclone-1.0.0-1.x86_64.rpm` 成功
  （136 个依赖，`--setopt=install_weak_deps=False` 避免容器镜像无关包）；落点与权限正确，
  `desktop-file-validate` 退出 0；以 uid 1000 非 root 运行 `appclone version` → `1.0.0`，
  `doctor` 判定 GTK 4.16.13 / libadwaita 1.6.1 / `gio` 正常（关闭弱依赖时 fuse/strace 缺失按预期 fail）。
  **顺带硬化**：手动测试时曾把 arm64 二进制装进标着 x86_64 的包（nfpm 不校验 payload 架构）。
  发布流程在 stage 后新增 `file` 架构断言（`x86-64` / `ARM aarch64`），Arch 包也单独断言 amd64。
- **Arch 容器实装**（2026-11-08）：`pacman -U appclone-1.0.0-1-x86_64.pkg.tar.zst` 成功，
  `pacman -Ql` 落点正确；非 root `appclone version` → `1.0.0`，`doctor` 判定 GTK 4.24.1 / gio /
  bubblewrap 0.13.1 正常。fuse/strace 未装属预期（Arch 无 `optdepends`，README 与 doctor 已提示）。
  三个发行版族的包至此都在真实发行版容器里装过并跑过 CLI/GUI 冒烟中的对应部分。
- **对话框响应路径补测**（2026-11-08）：新增 `TestGtkDialogResponses`。产品在 `Choose` 前把
  对话框交给 `dialogHook`（生产恒为 nil），测试据此断言重命名/删除的默认响应是「取消」、
  删除响应是 destructive，并走**同一个** `u.cb.dialogFn` 触发产品回调：重命名经 Entry 改名为
  `工作号` 并落盘；删除先点取消、300ms 后实例仍在，再确认删除后实例目录清空（进回收站）。
  闸门 16 → **17 条**；另加 `TestGtkCloseSwitchDialog`（运行中提示框 + 删除全部确认框），
  合计 **18 条**。
- **CI 增加安装包构建 job**：每次 push 用固定版本 nfpm 真出 amd64 的 deb/rpm/Arch 三包，
  校验 stage 二进制架构、deb `Architecture`/`Depends`/`Recommends` 与 data 里的二进制/桌面入口/图标；
  release 复用 ci.yml 后同样受这道闸门保护，打包配置不再只在打 tag 时才被验证。
- **第三次只读复审后的收口**（2 P2 + 若干 P3，2026-11-08）：
  ① **P2：开关对话框用例没有真正验证回弹**——原用例 `row.SetActive(true)` 让 `revert` 的
     `SetActive(true)` 成为无操作，且只在响应后断言 `pending`。现改为模拟用户拨到「关」
     （`SetActive(false)`），响应前断言 `pending` 为 true，响应后断言 `pending` 清空**且**
     `row.GetActive()` 已回弹为 true；并断言运行中提示框没有 `cancel`/`trash` 响应。
  ② **P2：CI 的 deb 断言太松**——`grep -E "^(Architecture|Depends|Recommends):"` 只要有一行就能过，
     data 清单是"三者有其一"。改为逐字段/逐路径精确断言（`grep -qx 'Architecture: amd64'`、
     `Depends` 必须含 bubblewrap/desktop-file-utils/libglib2.0-bin、`Recommends` 必须含
     fuse-overlayfs/strace、四个落点逐个 `grep -qx`），并对 stage 的**每个**二进制单独做架构断言
     （`ci.yml` 与 `release.yml` 都从 `dist/stage/*` 通配改为逐文件循环）。
     已用固定版本 nfpm 本地重出 deb 并逐条执行这些 grep（全部命中，含 `./usr/...` 前缀），
     不是只改 YAML。
  ③ P3：测试构建标签统一为 `linux && gtk_e2e`；`defer d.Close()` 会补发 close-response 的注释写明
     （处理器表首次分派即删除，不会二次进入产品逻辑）；STATUS 中「16/16 / 约 30s」的旧快照改为
     **18/18 / 约 50s**，`workflow_call` 的 job 枚举补 `packages`；对话框用例的删除结果表述改为
     「实例从列表消失」（回收站由 core 的 `gio trash` 路径负责，本用例不直接断言回收站内容）。
  **记录不修**：不用 `gobject.SignalEmitByName` 走真实信号——purego 的 variadic 直接把 Go 值当
   C varargs 传，GValue 编排错误时会静默调用错误处理器；`u.cb.dialogFn` 与 `ConnectResponse`
  绑定的是同一个产品处理函数，信号接线本身由其余 GTK 用例的真实 `button-clicked`/notify 覆盖；
  `dialogHook` 也不加构建标签——它只是一个恒为 nil 的函数变量，隐藏它需要复制一份 `Choose` 调用点，
  收益不抵复杂度（生产二进制里没有赋值方）。
  证据：`gofmt -l .` 空、`go vet ./...` 与 `go vet -tags gtk_e2e` 干净、`go test -count=1 ./...` 全绿、
  `staticcheck ./...` 零输出、`node design/guitest.js` **62/62**、`design/guitest-gtk.sh` **18/18 PASS**
  （退出码 0）；本地 nfpm 出包后 CI 的每条 deb 断言均实跑通过。
- **CI 兼容性预演（ubuntu:24.04 / libadwaita 1.5）**（2026-11-08）：用容器跑同一份
  `design/guitest-gtk.sh`，抓到测试辅助函数用了 `adw.PreferencesGroup.GetRow`——
  `adw_preferences_group_get_row` 在 libadwaita **1.5 不存在**（**1.8** 才加），puregotk 注册出的
  函数指针为 nil，用例直接 panic；首次 CI 的 gtk job 本来必红，本机 GTK 4.24 从来跑不出来。
  改为在控件树里找 `GtkListBox`、按 `title` 认行（`typeNameOf` 读 GType 名），不再依赖 1.8 API。
  预演结果：非 root 下 **17/18 PASS**；唯一失败的 `TestGtkRapidRepush` 是嵌套容器不允许
  探测用 bwrap 挂 `/proc`（`Can't mount proc on /newroot/proc: Operation not permitted`），
  预计在 GitHub 的 VM runner 上不成立（待首次 CI 实跑确认），不是代码缺陷。
  顺带给两个 GTK step 加 `GSK_RENDERER=cairo`（容器里非 root 跑 Xvfb 时 GLX 报过 `BadAccess`；
  闸门不需要 GL 渲染）。证据：宿主机（GTK 4.24 / libadwaita 1.10）重跑 **18/18 PASS**、退出码 0。
  同一最终树再跑 `design/probe-smoke.sh`：退出码 0（探测 → 档案 → 实例 → 审计 → verify → 回收站，
  公共目录哨兵保留、无残留挂载/写层）。另从**干净 clone**（只含已提交文件）重跑
  `CGO_ENABLED=0 go build ./...`、`go test ./core/... ./cmd/appclone` 与 nfpm 出 deb，全部通过，
  排除「本机能过是因为有未提交文件」的可能。针对四条 findings 的修复经只读复审 **APPROVED**（0 P0/P1）。**记录不修**：遍历中未命中的
  兄弟控件与 `GtkListBox` 包装仍不 `Unref`（与 `findWidgets` 既有约定一致，测试进程内引用、
  退出即回收；puregotk 无 finalizer）；`9411453` 提交正文的「这两个版本」措辞应读作「1.8 之前」，
  已推送的历史不改写。
- **strace 降级真机校准（2026-11-08）**：在假 `HOME` 下把 `PATH` 去掉 `fuse-overlayfs`（保留
  `strace`），`appclone add stracedemo` 退出码 0；两轮真实日志经 `ParseStrace` 产出
  `paths = ["~/.config/StraceDemo"]`、`probe_method = "strace"`，写入只落在假 HOME 的该目录，
  `ignored` 四类与 keyring `unsupported` 按契约写进档案。此前"未校准"的判断来自**在外层 strace
  里再跑**（嵌套 tracer 报 `PTRACE_TRACEME: Operation not permitted`），不是产品路径的缺陷。

**第三十八轮：GUI 分身生命周期解耦（ADR-028）**（2026-11-08）。用户拍板：**分离运行 + 停止按钮**。
- GUI 点「启动」不再在自己的进程里跑 `core.Run`，而是以自身二进制 fork 一个会话首进程（runner，
  `APPCLONE_GUI_DETACH_APP/ID` + `Setsid`）；runner 在 GTK 初始化前分流，复用与 CLI 同一条
  `core.Run`。关掉设置窗口只结束 GUI，正在运行的分身继续跑。runner 是一次性进程，不监听、
  不提供 API——**不是 daemon，也不引入 IPC**（ADR-004 不变）。
- `core.Run` 拿锁后写 `<inst>/run.pid`（`pid` + `/proc/<pid>/stat` 启动时刻，tmp+rename 原子替换），
  退出时**先删记录再放锁**；运行态判定仍只看 `run.lock`（ADR-023 不变），pid 文件只用于定位
  停止信号的目标。
- 新增 `core.StopRun` 与 CLI `appclone stop <app> <id>`：先用 `run.lock` 证明实例在跑，再校验
  启动时刻防 PID 复用，最后 SIGTERM 给 runner（由 `core.Run` 的整组 `SIGTERM`→3s→`SIGKILL`
  收尾）。锁拿得到按 stale 清理，且清记录在**持锁状态下**做，避免误删刚启动的新 run；
  runner 在注册信号处理器之前就被默认 SIGTERM 打死时，锁是否可拿是唯一可靠判据（残留记录一并清）。
  未运行/无法确认目标 ⇒ 退出 7，绝不盲杀 PID。
- GUI 详情页运行中把「启动」换成「停止」，二次确认默认「取消」（未保存内容可能丢）；
  停止成功/失败走 toast；runner 始终 `Wait` 收尸。启动输出追加到 `run.log`（**每次封顶 1 MiB**，
  超限静默丢弃；启动失败只把**尾部一行**清掉前缀/非法 UTF-8/控制字符后进 toast，不把日志路径
  或原始 OS 错误甩给用户；2 秒内退出的 runner 才判「启动失败」，否则报「已退出」）。
  删除按钮在运行中禁用，tooltip 明说「先停止才能删除」。
- 交互基线同步：HTML 稿运行中显示「停止」、第三次破坏性确认，`guitest.js` **58 → 60**；
  GTK 闸门新增 `TestGtkDetailShowsStopWhenRunning`、`TestDetachedRunnerSpawn`（env 传递 +
  Setsid + 收尸）、`TestDetachLogHelpers`（封顶不短写 + 日志清洗）、`TestDetachedTargetEnv`（标记分流），共 **13 条**。
- **对抗复审修复（P2-1/P2-2）**：① 信号处理器提前到 `Run` 入口（`stopping` atomic +
  缓冲 pgid），启动窗口内收到 SIGTERM 不再走默认处置——不再残留 `run.pid`、不再留孤儿
  `dbus-daemon`；私有 dbus 加 `Pdeathsig`；写入 `run.pid` 失败降级为警告并继续启动（只是
  本次不可 stop）。② `run.log` 封顶 1 MiB + UTF-8/控制字符清洗，GUI 侧不再传原始错误与路径。
  顺带修：stop 的 ENOENT 分支在持锁状态下清 stale 记录（避免误删刚启动的新 run）、
  pidfd + 二次启动时刻校验（收口 check→kill TOCTOU）、core 测试补锁占无 PID 与
  「进程死了但收尾没跑」两条。
证据：`gofmt -l .` 空、`go vet ./...` / `go vet -tags gtk_e2e` 干净、`go test -count=1 ./...`
全绿（`go test -race ./core/... ./cmd/appclone` 亦绿）、`CGO_ENABLED=0 go build ./...` 通过、
`node design/guitest.js` **60/60**、`design/guitest-gtk.sh` **13/13 PASS**（4.32s）；假 HOME 真机冒烟：
GUI 二进制 runner 模式启动 sleep 分身 → `appclone stop` **0.2s** 返回 0、runner 退出、`run.pid`
清除、running=false、无孤儿进程；CLI `run`+`stop` 同链路通过，二次 `stop` 退出 7。
**启动窗口压力冒烟**：1500 条 paths 把「启动→应用起来」拉到 733ms，在 150–650ms 六个时点各杀
两次（12 次中 10 次应用尚未启动、2 次已启动）——零残留 `run.pid`、零占锁、零 dbus/bwrap 孤儿，
早到信号下 runner 以 0 退出且应用不再被拉起。
**delta 对抗复审：APPROVED（0 P1/P2，9 条 P3）**。已收口：私有 DBus 地址读取加 2 秒上限
（`err.env.dbus_timeout`，假 daemon 回归 `TestPrivateDBusAddressTimeout`，堵住"setup 卡死→stop
也停不掉"的残留窗口）；`run.log` 按本次启动 offset 读尾部（不再把上一轮最后一行当本次原因）；
用户主动停止后抑制多余的「已退出」toast；文档修正 `run.log` 安全表述与 §6.1 详情页布局；
ESRCH 快速返回不持锁删 `run.pid`（防止误删新 run 记录）的注释固化；GUI runner 注释改为
"GTK 初始化前分流，但 puregotk init 仍 dlopen libgtk"；补 `TestDetachedTargetEnv`。
**记录不修**：停止确认对话框的响应路径仍未自动化（第四十一轮已用 `dialogHook` 覆盖删除/重命名/
关闭开关三处，停止确认框仍未接；见第三十八轮）；`cappedWriter` 保留头部 + toast 读尾部的组合在"应用先灌满 1 MiB
再失败"时只能给 best-effort 原因（已在注释与 STATUS 写明）。

**第三十七轮：GTK 闸门补四态 + GUI 文案入 i18n（ADR-027 退场）**（2026-11-08）。
闸门新增五条用例，凑齐三屏四态（列表/配置/详情 × probing/confirm/creating/error）：
- `TestGtkSetupStates`：拨开关进入首次配置 → 同步落在 `probing`（确认按钮隐藏、取消按钮叫
  「取消探测」）→ `finish(false)`（取消按钮绑的同一个函数）→ 页面 pop、无实例、无档案。
  同一条用例还断言降级横幅：`noFuseMounter` 下横幅必须真的显示；发出真实
  `button-clicked` 信号（走产品 `ConnectButtonClicked` 接线，不是直调回调）后消失，
  且后续 `applyEnvBanners` 不能把它弹回来。
- `TestGtkErrorState`：预置版本超前档案走独立窗口的 `open` 入口，等页面真的可见后断言
  `error` 态（标题按原因「档案由更新版本生成」、描述非空、**不显示重试**、取消按钮叫
  「取消」）。独立窗口避开 pop→push 同名 tag 的过渡态，不靠 sleep。
- `TestGtkFatalBanner`：空 PATH 让 `CheckBwrap` 失败 → 致命横幅显示、降级横幅不显示、
  搜索停用、应用开关停用。
- `TestGtkRetryableError`（对抗复审补）：可重试错误态下「重试」必须可见、可点、文案是
  「重试」，点它要走产品接线把重跑回调真的触发；取消后页面关闭。
- `TestGtkRapidRepush`（对抗复审补，P1 回归）：push 配置页 → 可见后立刻取消 → **同一个
  tick** 再 push 一次。`AdwNavigationView` 在 pop 过渡期间仍持有旧页 tag，原先写死的
  `"setup"`/`"detail"` 会让第二次 `Push` 被静默拒绝（只在 stderr 留一条
  `Adwaita-CRITICAL: Duplicate page tag`），用户看到开关拨了没反应。产品侧改为
  `pushPage` 给每页发唯一 tag（`page-<seq>`）；把 tag 改回固定值并删掉 `strconv` 后
  该用例在 20 秒超时红（已实测），证明它真的守着这个竞态。
- 生命周期用例补 creating 断言：确认按钮发出真实 `clicked` 信号后必须立即进入 `creating`
  （确认/取消按钮同时停用、`GetCanPop()` 为假），不再只看「实例最终落盘」。
各条新断言都做过变异验证（失败态显示重试 / 致命态开关仍可点 / 固定 page tag /
`newUI` 写死 zh_CN，分别立即变红）。
三屏静态文案（约 150 条 `ui.*` 文案、160 处 `tr()` 调用）全部进 `core/i18n`（zh_CN + en），`newUI` 统一
`i18n.Lang()`；新增 `core/i18n/i18n_test.go`：两语言键一一对应、占位符集合一致、
en 表不得混入中文、`DetectLang` 顺序与简体边界、缺键返回键本身。另加
`TestCoreKeysExist` 扫 `core/*.go` 里所有字面量 `err.*` / `probe.why.*` 键并在两张表里逐个核对，
`TestViewLiteralKeysExist` 再扫 `cmd/appclone-gui` 与 `cmd/appclone` 的
`ui.*`/`err.*`/`probe.why.*`——
只比两张表发现不了「代码新加 key、表没跟上」导致界面直接显示原始键名。表一致性测试
**顺手抓出两个真实缺陷**：`err.profile.exec_field_code` 与
`err.profile.extra_bind_missing` 只有 en，中文界面会回落成英文（已补 zh）；缺键扫描又补上
`err.env.ac_home_relative`、`err.internal.probe_spec`、`err.probe.mount_timeout`。
切片参数的分隔符也改成跟语言走（zh「、」/ en「, 」），`ui.toast.separator` 单列
（zh「；」/ en `"; "`），避免英文界面出现顿号。`TestGtkEnglishStrings` 用
`LC_ALL=en_US.UTF-8` 走 `newUI`，断言窗口标题「App Clones」、空态「No clones yet」、
降级横幅与行副标题都是英文、已抽检的控件文本里不残留 `ui.` 键名（把 `newUI` 写死
`zh_CN` 会立即变红；全量键名遗漏由上面的源码扫描兜底）；另加一条单测钉住「渲染只扫
模板一遍」：参数值里的 `{n}`/`$1` 必须原样保留，不能二次替换。
证据：`gofmt -l .` 空、`go vet ./...` / `go vet -tags gtk_e2e` 干净、
`go test -count=1 ./...` 全绿（含 i18n 表一致性）、`CGO_ENABLED=0 go build ./...` 通过、
`node design/guitest.js` **58/58**、`design/guitest-gtk.sh` **9/9 PASS**（最终树连续 3 次
4.13–4.21s；本轮闸门累计 11 次全绿）。**闸门偶发 `Test I/O incomplete` 的根因与修复**：取消探测后测试立即返回，
测试进程会在 `terminateGroup` 的 3 秒 SIGTERM→SIGKILL 窗口里退出——goroutine 的 defer 没机会
跑完，`dbus-daemon`/`strace`/`bwrap` 变成孤儿（`strace` 默认忽略 SIGTERM），其中继承的
stderr 正是 `go test` 的输出管道，18 秒后升级成闸门失败。修复是让 `TestGtkSetupStates` 与
`TestGtkRapidRepush` 在退出前等 `v.held == false`（探测 goroutine 真正返回并 release 之后
才为假），cleanup 完成才结束测试；修复前残留进程实测存在了 17 分钟，修复后累计 11 次连跑零残留。
**对抗复审 P1（存量缺陷，本轮收口）：取消/超时时在 overlay 还挂着的状态下
`os.RemoveAll(work)`**。`probeRoundFailed` 原先在 `unmount` 之前清理工作目录；`merged`
是 fuse 挂载点，`RemoveAll` 会先 `opendir` 再逐项 unlink，等于把整个 `$HOME` 视图遍历一遍，
upper 里探测期写下的文件（可能是用户刚保存的内容）被删，最后才因 EBUSY 报错；同时
dbus-daemon 还活着、进程会卡在遍历里不退。修复：`probeSession` 内统一 `teardown`
（停会话进程 → 停私有 dbus → 卸载 → 处置目录）；卸载后先做污染检查，只有公共目录进了
upper 或 upper 读不出来才 `quarantine`，并把恢复路径写进错误（取消用
`err.probe.canceled_kept`，CLI 打一行、GUI 弹 toast；超时无现场用
`err.probe.timeout_no_scene`）；只有第 0 轮 guard 在 exec 前失败才允许无条件删写层；
`discard`/`quarantine` 都带 `mounted` 硬护栏。回归：`TestProbeSessionCancelUnmountsBeforeCleanup`（fake 挂载在
`merged` 放哨兵文件，顺序错了哨兵先消失）。第二轮复审又收口三处：strace 降级没有写层现场，
超时改用独立键 `err.probe.timeout_strace`（不复用带 `{quarantine}` 的 overlay 文案，否则
界面会显示字面量）；空 upper 的取消不再留空 quarantine（读得出来且为空才删，读失败仍按
「可能有内容」保留）；guard 失败在 `round > 0` 时改为保留现场（前几轮可能已写入用户文件，
只有第 0 轮在 exec 前失败才允许删）。顺带收口：`startProbeBus` 失败路径补 `Wait`
并改用 `cmd.Wait()` 关掉 `StdoutPipe`（原先每次探测漏一个 fd、失败时留僵尸），
dbus-daemon 加 `Pdeathsig: SIGTERM`（GUI 被信号打死时私有总线跟着收），`setsidRunner`
在 `Start` 前查 `ctx.Err()`（取消后不再把应用拉起几毫秒），`pushDetail` 只复用仍是可见页的
详情页，`setupView.start/probe/submit` 增加 `done` 守卫（退场动画里的点击不再重跑事务）。
**已知策略**：`display_name` 的默认值「分身 N」是用户数据，不随界面语言翻译（ADR-025）；
英文界面首次创建仍显示「分身 1」，这是有意选择，若要中性默认名需单独定案。
**未覆盖**：删除/重命名/关闭开关对话框的
响应路径（第四十一轮已由 `dialogHook` 覆盖）；真实指针/键盘注入已由第十九条 GTK 用例覆盖（2026-11-09）。**Esc 竞态已结构性收口**
（2026-11-09）：不再假设窗口级控制器一定先于 `AdwNavigationView` 的快捷键控制器——直接
`SetPopOnEscape(false)`（1.4+），页面导航不再有第二个 Esc 处理者（对话框/搜索框仍各自处理）；闸门断言 `GetPopOnEscape()==false`，
GTK 18 条全过。真实按键注入现由 uinput 用例覆盖（见 `TestGtkRealInputUinput`），语义仍不依赖事件传播顺序。
**许可证已定**（2026-11-08，GPL-3.0-or-later，ADR-030）：`LICENSE` 入仓、nfpm 写 SPDX 标识符、
三格式包随附许可证文本；AUR 与发行版官方仓库的许可证前置条件不再缺失（AUR 按 2026-11-09 决定不提交，见 ADR-031）。
**首次 CI 已全绿**（2026-11-09 run #16 / `6210cf3`）：Go/packages/HTML/GTK amd64/GTK arm64 五个 job
全部成功；arm64 由 `ubuntu-24.04-arm` 真机 runner 跑通，打包行升 `verified`。

## 下一步

功能面已收口：探测（overlay + strace 兜底）→ 档案 → CLI → GUI 三屏 → 克隆起步 → deb/rpm/Arch 包
→ CI/发版流程 → 许可证都已落地。**首次 CI 已全绿（2026-11-09 run #16 / `6210cf3`）**：Go、packages、
HTML、GTK amd64、GTK arm64 五个 job 全部成功（1m 0s / 51s / 42s / 1m 15s / 50s），
`ubuntu-24.04-arm` 真机 runner 验证了 arm64。打包行已升 `verified`，arm64 阻塞解除。
剩下的按优先级：

1. **首次发版已完成（2026-11-09，v1.0.0）**：`release.yml` 的 validate → verify（复用整套
   `ci.yml`）→ release 全绿，Releases 上有 5 个安装包 + 2 个 tar.gz + `checksums.txt`
   （另 GitHub 自动附 2 个源码包，共 10 个资产）；证据见 `docs/STATUS.md` 打包行。
   actions 供应链已钉 commit SHA。**卸载回收 AppArmor profile 已完成**（2026-11-09）：
   `packaging/postremove.sh` 只删带 `appclone-owned:` 标记的 `/etc/apparmor.d/bwrap`，
   三格式包都带该钩子；bwrap 矩阵 + 真 `dpkg -r` 容器实测通过，证据见 `docs/STATUS.md` 打包行。

2. **自动化覆盖已补齐**（2026-11-09）：真实指针/键盘由 `TestGtkRealInputUinput` 覆盖——
   `/dev/uinput` 创建虚拟触摸屏/键盘（Wayland 原生、无需 root），触摸点切换器双向、点击聚焦
   搜索框、按键过滤与退格恢复；每次注入前复查窗口焦点。第二十条 `TestGtkRealInputStartStop`
   用真实触摸点击「启动/停止」，走 detached runner → `core.Run` → `run.lock` → `core.StopRun`
   完整链路（被测 Exec 为假 HOME 里的 `sleep 30` 脚本），确认框经 `dialogHook` 交出、测试以同一
   `dialogFn` 投递 `stop`；本机 5/5 PASS、一次点击即出框，`design/guitest-gtk.sh` 20 条全过。
   CI runner 无 `/dev/uinput` 时两条用例按预期 SKIP（门禁会检查 SKIP 行）。Esc 已由
   `SetPopOnEscape(false)` 结构性收口。
   **AUR 按用户决定不做**（2026-11-09）——只维护 GitHub Releases。

3. **CI #18 的一次性 job 取消已核对，不改 workflow**：packages job 在 `actions/setup-go`
   解压缓存阶段收到 `The operation was canceled.`（41s），同 run 其余四个 job 均完成；
   CI #18 在 Release #1 启动前约 8 分钟就已结束，二者没有重叠，**不是**并发组互相取消。
   该 job 单独 re-run 48s 全绿，未复现；原因未定位，按一次性事件记录（见 STATUS 打包行），不改 workflow。

## 被挡住的

- ~~`/dev/fuse` 不可用~~ **已解除**（2026-11-07 本机出现 `/dev/fuse`，fuse-overlayfs 1.18 / fusermount3 3.18）：
  `design/probe-smoke.sh` 已跑通整条真机链路。测试脚本自身曾有两处 fixture 缺陷（XDG 根放在假 HOME 之外、
  `run --audit` 的 flag 写在位置参数之后），已修。
- ~~**CI 首次实跑无法确认 / arm64 未验证**~~ **已解除（2026-11-09）**：run #16（`6210cf3`）五个 job
  全绿（Go / packages / HTML / GTK amd64 / GTK arm64），`ubuntu-24.04-arm` hosted runner 实跑
  GTK 闸门 50s 通过，ADR-016 的 arm64 要求达成。首轮红是 gofmt 1.26/1.27 缩进分歧（`core/audit.go`），
  拆分返回字面量后消除；STATUS 打包行已升 `verified`。
- ~~**`LICENSE` 未定**~~ **已解除（2026-11-08）**：用户拍板 **GPL-3.0-or-later**（ADR-030）。仓库根
  新增 `LICENSE`（GPLv3 全文，sha256 `3972dc…`，与 automake/R 的 GPL-3 副本逐字节一致）、
  `packaging/copyright`（DEP-5）与 `THIRD_PARTY_NOTICES`；nfpm `license: GPL-3.0-or-later`，
  deb 落 `/usr/share/doc/appclone/{copyright,THIRD_PARTY_NOTICES}`、rpm/Arch 落
  `/usr/share/licenses/appclone/{LICENSE,THIRD_PARTY_NOTICES}`；本地重出三格式包核对通过。
  许可证不再阻塞任何分发渠道；**AUR 按用户决定不做（2026-11-09，ADR-031）**，只维护 GitHub Releases。
- ~~**strace 降级未用真实日志校准**~~ **已解除（2026-11-08）**：把 `PATH` 去掉 `fuse-overlayfs`
  （保留 `strace`）后在假 HOME 跑 `appclone add stracedemo`，退出码 0；两轮真实日志产出
  `paths = ["~/.config/StraceDemo"]`、`probe_method = "strace"`，写入只落在假 HOME 该目录。
  旧结论的根因是**在外层 strace 里再跑**（嵌套 tracer 报 `PTRACE_TRACEME: Operation not permitted`），
  不是产品路径问题；正常用户不会在外层 tracer 下运行 appclone。
- **GUI 侧的自动化闸门**：`guitest.js` 只覆盖 HTML 稿；GTK 实现现由 `design/guitest-gtk.sh`
  覆盖**页面生命周期与三屏四态**（列表/确认/创建/详情/返回 + 首屏落点、空态映射、
  探测中取消/失败/可重试错误态、快速重推竞态、降级与致命横幅、en 文案，2026-11-08 起），但**指针与键盘事件注入仍不可行**
  （本会话 XTEST 被禁，xdotool 指针/键盘无效），所以删除/重命名/关闭开关对话框的响应路径
  当时还不能脚本化（第四十一轮以 `dialogHook` 补上）。broadway 的结论保留在下方，供后续参考。
  `gtk4-broadwayd` 在 `NavigationView` push 后会以其自身断言
  （`gdk/broadway/broadway-output.c:458 append_node_ops: !reused_node->consumed`）**自崩**，
  `/tmp/.X11-unix` 下无 Xvfb，嵌套 `mutter --headless` 能起 Xwayland 但合成器重定向使
  root 抓图全黑。可行方向：装/借一个 Xvfb（或 `GSK_RENDERER=cairo` + 独立 X server），
  或在 GUI 内加 `APPCLONE_GUI_SHOT` 这类**仅测试**的 `GtkWidgetPaintable → png` 出口。
  **本机重试的新发现**（2026-11-08，GTK 4.24.1）：`broadwayd` 不带 `-u` 时监听**抽象** socket
  （`@/run/user/<uid>/broadway<N>.socket`），而 GDK 客户端找文件系统路径 → `Failed to open display`；
  带 `-u` 时 HTTP 端口根本不监听（`-u` 替换了 TCP 监听）。用 socat 把文件系统 socket 桥到抽象 socket
  能连上，但 GDK 立刻报 `Unknown input command d/w` 并 EOF（协议桥接不可靠）。另外本会话的 X 会话
  禁止 XTEST 注入（`xdotool mousemove` 不移动指针），所以**需要真实点击**的交互（对话框、
  探测中取消）仍无法脚本化——页面生命周期已改由 `design/guitest-gtk.sh` 覆盖。
  可行路线：装 Xvfb（或换一台能跑 Xvfb 的机器）、在 GUI 内做 `APPCLONE_GUI_SHOT` 出口，
  或把三屏的纯逻辑状态机从 GTK 里抽出来做无显示测试。不需要输入的 GTK 生命周期已由
  `design/navspike`（导航页 Unref 时机）与 `design/guitest-gtk.sh`（列表 → 确认 → 创建 →
  详情 → 返回 + 三屏四态/横幅/en 文案）覆盖；**未覆盖部分**（第四十一轮前的删除/重命名/关闭开关对话框；
  真实指针/键盘注入）在写下时不能声称已验证。**2026-11-09 已解除**：删除/重命名/关闭开关
  对话框经 `dialogHook` 覆盖，真实输入由 `TestGtkRealInputUinput` 与 `TestGtkRealInputStartStop`
  覆盖（Wayland + 显式 `APPCLONE_GTK_UINPUT=1`），本条仅保留当时的环境限制记录。
- 复现命令（broadway 冒烟，假 HOME）：`gtk4-broadwayd -p 8095 :5` +
  `GDK_BACKEND=broadway BROADWAY_DISPLAY=:5 HOME=<fake> AC_BASE=<fake>/xdg/appclone ./appclone-gui`，
  浏览器打开 `http://127.0.0.1:8095/`（注意 `XDG_RUNTIME_DIR` 必须是 broadwayd 所在的那个，
  否则客户端找不到 `broadway*.socket`；单个 broadwayd 只接一个应用实例时最稳）。
  **本机不可直接用**：见上一条——抽象/文件系统 socket 不匹配，`-u` 又关掉 HTTP。

- ~~**（未决）GUI 启动的分身与 GUI 进程同生共死**~~ **已决并落地（2026-11-08，ADR-028）**：
  GUI 以自身二进制 fork 会话首进程 runner 跑 `core.Run`，关设置窗口不再结束分身；详情页运行中
  提供「停止」（CLI 同步新增 `appclone stop`），停止链路与 CLI 共用 `core.StopRun`。
  证据见第三十八轮。

## 最近完成

- 2026-11-09 **真实点击启动/停止 + v1.0.0 升级回收 + 跨发行版/性能证据**：
  ① 新增第二十条 GTK 用例 `TestGtkRealInputStartStop`：uinput 虚拟触摸屏真实点击详情页
  「启动」与「停止」，事件经 libinput → GDK → 按钮，走 detached runner → `core.Run` →
  `run.lock` → `core.StopRun` 全链路；被测 Exec 是假 HOME 里的 `keepalive.sh`（写自己的配置后
  `sleep 30`），失败也由 defer `StopRun` 收尾。确认框经 `dialogHook` 交出并断言默认响应 `cancel`，
  测试用同一个 `u.cb.dialogFn` 投递 `stop`。本机单次约 2.8s、连续 **5/5 PASS**（每次 1 次点击
  即出框），`APPCLONE_GTK_UINPUT=1 design/guitest-gtk.sh` **20 条全过 / 退出码 0**。
  真实输入门禁在本轮收紧为 Wayland 原生：`APPCLONE_GTK_UINPUT=1` 时脚本强制
  `GDK_BACKEND=wayland` 并把 `WAYLAND_DISPLAY` 转成绝对路径（用例把
  `XDG_RUNTIME_DIR` 指向假 HOME），用例内再用 GDK display name 复核，且检查先于
  首次校准点击；完整闸门在本机连续 5/5、20 条全过。用例还断言应用写出的 `state`
  只落在实例根、宿主假 HOME 无该文件（只查存在性）。顺带修掉 `TestGtkRapidRepush`
  的 X11 时序假设：第二页可见时探测可能已完成到 error，断言改为「新 view + 可见 +
  进入 probing/confirm/error 之一」，固定 tag 回归仍由 `second != first` 与可见性钉住。
  调试中确认产品侧 `clickedFn` 每次都执行、确认框在点击后约 52ms 已交给测试；此前的失败是
  测试自己把对话框塞回通道、继续补点并最终走到临时「直接调用处理器」诊断分支造成的假阴性，
  把对话框塞回通道的诊断分支与「直接调用处理器」旁路已删除；落点重试（最多两次）保留。② `postinstall` 增加 v1.0.0 无标记 profile 迁移：只在目标内容与
  模板去掉标记行后逐字节一致时补写标记；`postremove` 改为 `apparmor_parser -R` best-effort 后
  **无论成败都删文件**；同时把 deb `postrm` upgrade 参数、rpm `%postun` 参数 1、Arch
  `post_remove` 仅在 `newpkg==NULL` 时执行的语义写进注释与 ADR-016；nfpm 顶层
  `archlinux.scripts.postupgrade` 接同一迁移脚本（pacman 升级只跑 `post_upgrade`），
  重出包后解包确认 `.INSTALL` 含 `post_install`/`post_upgrade`/`post_remove` 三段；提交 `bcbb383`
  的 CI run 42650211815 五个 job 全绿（Go 52s / packages 28s / HTML 30s / GTK amd64 48s /
  GTK arm64 46s，总 54s）。`ubuntu:24.04` 容器真
  `dpkg` 实测 `0.0.0~test`（无标记）→ 升级 `0.0.0~test2` → `dpkg -r` 后文件被回收
  （`DPKG-UPGRADE-RECLAIM PASS`），异内容无标记文件两侧都不碰。③ 跨发行版冒烟（假 HOME，
  每环境 3 轮 add→run→list→rm，检查宿主零污染、公共目录共享可见）：Arch 宿主、Fedora 41、
  Arch 容器、Ubuntu 24.04 容器全部 3/3 通过。④ 性能基线（本机，假 HOME）：`BuildBwrapArgs`
  约 1.70ms/op（约 4874 次分配，主要花在 `EvalSymlinks`）、`LoadProfile` 约 36.7µs、
  `ListInstances(100)` 约 1.91ms；CLI 20 次 p50：`version` 3.3ms、`list --json` 27.0ms、
  `run` 110.5ms、`doctor --json` 5.9ms。新增 `core/bench_test.go` 固化前两项基准。
- 2026-11-09 **真实指针/键盘注入 + 卸载回收 AppArmor profile**：① `TestGtkRealInputUinput`
  用 `/dev/uinput` 创建虚拟触摸屏/键盘（与 ydotool 同机制，Wayland 会话原生可用、无需 root），
  事件经 libinput → GDK → 控件；覆盖视图切换器左右标签的触摸点击（双向）、触摸聚焦搜索框、
  逐键输入若干字符（不假设键盘布局）与逐键退格恢复。窗口全屏后注入，每次注入前复查 `GtkWindow:is-active`，
  失焦或退回非全屏即安全中止；首次注入前做校准点击（捕获相观察器）确认事件回到本窗口，全屏固定到桌面原点所在显示器，连发按键改为逐键发送并在每键前
  复查焦点，断言不假设键盘布局（按实际字符数退格）。门禁要求显式
  `APPCLONE_GTK_UINPUT=1` 且 `/dev/uinput` 可写才启用（默认关闭，避免嵌套合成器下误投）；输入后用 Return 提交
  IME 预编辑，并按产品归一化（ToLower/TrimSpace）断言 `u.query` 已由 search-changed 写入再退格。本机 2.9–3.1s PASS、连续 3 次，
  `design/guitest-gtk.sh` 19 条全过、退出码 0；CI 无 `/dev/uinput` 时按预期 SKIP 且
  门禁检查该行。**审查记录**：本次 Claude subagent 三次启动（含窄范围）均未在 7–11 分钟内
  返回，按规则记录为阻塞；改用本机 Codex 只读审查（`codex exec review --uncommitted`），
  提出 1 P1（门禁强制开启注入、覆盖调用者显式关闭）+ 2 P2（连发按键间不复查焦点、
  键盘布局假设），三条均已修复并复跑门禁。② `packaging/postremove.sh`：只删带
  `appclone-owned:` 标记的 `/etc/apparmor.d/bwrap`，先 `apparmor_parser -R`，失败保留（**同日已改为失败也删文件**，见上一条与本轮记录）；
  deb/rpm/Arch 三种包均带钩子（deb `postrm`、rpm 脚本、Arch `.INSTALL` 的 `post_remove`）。
  bwrap 矩阵八种情况全过（含 rpm/deb 升级参数守卫），`ubuntu:24.04` 容器里 `dpkg -r` 实测自己的文件删除、无标记文件保留；`dpkg -i` 跨版本升级时文件保留。
- 2026-11-09 **v1.0.0 发布与 CI #18 事件核对**：`v1.0.0` tag（指向 `9e0cf6f`）触发 Release #1，
  validate 4s、verify 的 Go 1m 7s / packages 44s / HTML 46s / GTK amd64 1m 11s / GTK arm64 43s、
  release 51s 全部成功（run https://github.com/mensylisir/appclone/actions/runs/42604108161）。
  Releases 页（https://github.com/mensylisir/appclone/releases/tag/v1.0.0）有 5 个安装包
  （deb/rpm × amd64/arm64 + Arch x86_64）、2 个架构化 tar.gz 与 `checksums.txt`，另有 GitHub
  自动生成的 2 个源码包，共 10 个资产；页面同时展示每个资产的 SHA-256（`checksums.txt`
  自身为 `d31d0f4a…`）。**CI #18（run 42602702293）的一次性取消已核对**：packages job 在
  `actions/setup-go` 解压缓存阶段收到 `The operation was canceled.`（41s），同 run 其余四个
  job 均完成；该 run 在 Release #1 启动前约 8 分钟就已结束（Release 页 18:50:03+08:00，
  CI #18 页面的 18:56:54 是其 re-run 时间），二者没有重叠，**不是**并发组互相取消。
  job 单独 re-run 48s 全绿且未复现；原因未定位，按一次性事件记录，不改 workflow 并发配置。
  改动推送后 CI #19（b67389c）、#20（2a952b4，撤回改动）与 #21（eedb6da，run 42607252858）
  连续三次五个 job 全绿；#21 耗时 Go 1m 0s / packages 55s / HTML 34s / GTK amd64 1m 34s /
  GTK arm64 56s。
- 2026-11-09 **Actions 供应链硬化**：`ci.yml`/`release.yml` 的 `actions/checkout`、`setup-go`、
  `setup-node`、`browser-actions/setup-chrome`、`softprops/action-gh-release` 全部从浮动 tag
  改为 commit SHA（行尾保留版本注释），避免上游 tag 被移动时 CI 执行到未知代码；三份 YAML
  解析通过。CI 首次全绿时 checkout/setup-go 实际解析到的正是这两个 SHA，行为不变。
- 2026-11-09 **首次 CI 全绿 + arm64 实跑验证**：run #16（`6210cf3`）在 GitHub hosted runner 上
  Go 1m 0s、packages 51s、HTML 42s、GTK amd64 1m 15s、GTK arm64 50s 全部成功
  （https://github.com/mensylisir/appclone/actions/runs/42502888804）。此前 15 次 CI 全红，
  根因不是依赖/GTK，而是 **gofmt 1.26 与 1.27 对「return 多个复合字面量」的缩进规则不同**：
  CI 用 go.mod 的 Go 1.26.0 判 `core/audit.go` 未格式化，本地 1.27 判通过。把两个字面量拆成
  `sess`/`layers` 再返回后两版 gofmt 都干净；Go 1.26.0 下 vet/test/staticcheck 全绿。
  STATUS 打包行 `implemented` → `verified`；arm64 阻塞解除（GTK arm64 job 实跑通过）。
- 2026-11-09 **Esc 语义结构性收口**：GUI 除窗口级 `GtkEventControllerKey` 外，
  调用 `AdwNavigationView.SetPopOnEscape(false)`（libadwaita 1.4+）。此前"窗口控制器先于
  nav 的快捷键控制器"依赖控件树传播顺序：焦点在子控件时 nav 可能先冒泡触发并 pop，
  与 §4.3「Esc 不承担返回」冲突。关闭后页面导航不再有第二个 Esc 处理者（探测/确认页取消、
  其余页什么都不做；对话框/搜索框仍按各自控件语义消费 Esc），Alt+← / 鼠标返回键 /
  触摸手势仍由 nav 原生处理。`TestGtkScreensLifecycle` 新增 `GetPopOnEscape()==false` 断言；
  `gofmt`/`go vet`/`go test ./...`、`design/guitest-gtk.sh` **18/18 / 退出码 0**、
  HTML 62/62。该轮写下时本机 XTEST 被禁、尚不能做真实指针/键盘注入；同日随后的
  `/dev/uinput` 用例（第十九、二十条）已补上真实触摸/键盘与真实点击启动/停止，语义仍不依赖事件顺序。
- 2026-11-08 **许可证定为 GPL-3.0-or-later（ADR-030）**：仓库根新增 `LICENSE`（GNU GPLv3 全文，
  取自 gnu.org，与本机 `/usr/share/automake-1.19/COPYING`、`/usr/share/R/licenses/GPL-3` **逐字节一致**，
  sha256 `3972dc…`；与 SPDX 副本只差 `(C)`/`©` 与 why-not-lgpl 链接写法）；另增 `packaging/copyright`
  （DEP-5：版权人 + "either version 3 … or any later version"）与 `THIRD_PARTY_NOTICES`（7 个静态链接
  第三方模块 + Go 标准库/运行时的许可证与专利授权全文；sha256 `e9218b9e…`）。`packaging/nfpm.yaml` 写 `license: GPL-3.0-or-later` 并按格式放置：
  deb → `/usr/share/doc/appclone/{copyright,THIRD_PARTY_NOTICES}`，rpm/Arch →
  `/usr/share/licenses/appclone/{LICENSE,THIRD_PARTY_NOTICES}`；`.tar.gz` 也带 `LICENSE` 与
  `THIRD_PARTY_NOTICES`。本地重出三包核对：deb control 有 `License:`、data 有 copyright/notices 且无
  `/usr/share/licenses/`，Arch `.PKGINFO` 有 `license =`、包内两份文件，rpm 载荷同 Arch（头字段由 CI 用
  `rpm -qp` 断言，本机无 rpm CLI）；SSOT 文件在三格式包内 sha256 与仓库一致。CI 的 packages job 扩展为
  逐格式断言（含 deb 负断言）。README 增加版权与许可证段，tech-stack §6/ADR 索引同步。
  这解除了 AUR 与发行版官方仓库的许可证前置条件（提交 AUR 仍需账号，未做）。
  **记录不修**：`appclone version` 与 GUI 不额外展示许可证——GPLv3 未强制要求，FSF 的交互式
  声明只是建议；源码文件也不逐一加 SPDX 头（GPLv3 不要求，README/包元数据已表达许可与 or-later）；
  deb 的 `THIRD_PARTY_NOTICES` 不 gzip——Policy 12.3 是 should 级，33KB 的声明保持可读更实用。
- 2026-11-08 **第四十轮：GUI 克隆起步入口 + 无图标首字母头像**：确认页「数据来源」开关
  （默认关、rediscover 隐藏）把 `--fork` 接进 GUI；无图标应用退 `AdwAvatar` 首字母色块
  （列表 32px / 探测中 64px），不再用 `application-x-executable`。HTML 基线 62/62、
  GTK 闸门 16/16（新增开关持久化、降级禁用与图标回退判定）。证据见第四十轮。
- 2026-11-08 **第三十九轮：克隆起步 `add --fork`（ADR-029）+ 窗口默认 880×720**：
  运行期 per-path `fuse-overlayfs`（lower=宿主数据、upper=实例 root、merged bind 给 bwrap），
  空/缺失路径跳过、失败降级不阻断启动、SIGKILL 残留按 mountinfo 先卸后删；真机冒烟证明
  首次继承 `SEEN=main`、分身写后宿主不变、二次 `SEEN=clone`，stale 挂载下一轮自愈。
  GTK 用例钉住默认窗口 ≥800×600。证据见第三十九轮；GUI 的 fork 开关留待下一轮。
- 2026-11-08 **第三十八轮：GUI 分身生命周期解耦（ADR-028）**：detached runner（GUI 自身二进制
  以 `Setsid` fork，复用 `core.Run`）+ `run.pid` 信号目标 + `core.StopRun` + CLI `appclone stop`；
  详情页运行中「启动」换「停止」（二次确认默认取消）；HTML 基线 58 → 60，GTK 闸门 9 → 13
  （新增停止按钮状态、runner 启动方式、run.log 封顶与清洗）。假 HOME 真机冒烟：停止 0.2s、
  无孤儿、二次 stop 退 7；启动窗口 12 次 SIGTERM 压测零残留（含 10 次应用尚未启动）。
  证据见上文第三十八轮。

- 2026-11-08 **第三十七轮：GTK 闸门补齐三屏四态 + GUI 文案入 i18n（ADR-027 退场）**：
  新增 `TestGtkSetupStates`（probing 取消、降级横幅「了解」）、`TestGtkErrorState`（error 态无重试）、
  `TestGtkRetryableError`（可重试错误态的「重试」接线）、`TestGtkRapidRepush`（pop 过渡中再次 push
  的导航竞态，产品侧以唯一 page tag 修复）与 `TestGtkFatalBanner`（致命态横幅/搜索/开关联动），
  连同生命周期与详情页用例凑齐列表/配置/详情 × probing/confirm/creating/error（9/9 PASS）；
  约 150 条 `ui.*` 文案（160 处 `tr()` 调用）按 `core/i18n`（zh_CN + en），`newUI` 跟随
  `i18n.Lang()`；表一致性 + core 键枚举测试抓出两个只有 en 的既有错误键与三个缺失键并补齐；
  `TestGtkEnglishStrings` 验证 en 界面。证据见上文第三十七轮。

- 2026-11-08 **第三十六轮：列表拆成两个平级视图（分身 / 所有应用）**：`AdwViewStack` + 标题栏
  `AdwViewSwitcher`，两页各自滚动；首屏按有无实例落点、创建后切回「分身」；「分身」页数量角标
  （1.10 实测当前页也显示）；空态只取代「分身」视图的列表，「选择应用」与切换器都通向
  「所有应用」。交互基线（HTML 稿 + 58 项回归）与 GTK 闸门（新增落点断言）同步。
  证据见上文第三十六轮。

- 2026-11-07 **第二十五轮：真机探测链路验证通过（L1 + 审计写入侧转 `verified`）**：
  - 本机出现 `/dev/fuse`，`design/probe-smoke.sh /tmp/appclone` **非 root** 一次跑通 9 步、退出码 0：
    doctor → 两轮 overlay 探测 → 档案（`probe_method="overlay"`、`paths=["~/.config/ProbeDemo"]`）→
    实例 → `run --audit` → `doctor --verify` = `verified` → `rm` 回收站。
  - 新增两条隔离断言（`7b`）：应用写入必须出现在实例 `root/` 内；宿主同名目录只能是空占位。
  - **修掉冒烟脚本两个把失败伪装成产品问题的 fixture 缺陷**：① XDG 根被放在假 HOME 之外，
    而 overlay 只罩 `$HOME`（归因必然零写入）；② `run probedemo main --audit` 的 flag 写在位置参数后，
    被 `SetInterspersed(false)` 当成透传参数（`run.toml` 根本没生成）。
  - 顺带发现：`fuse-overlayfs -o auto_unmount` 在 1.18 + libfuse 3.18 上被忽略（SIGKILL 后挂载点残留），
    已把代码注释与 `discover.md` 的承诺改成"尽力而为 + 启动时扫残留兜底"。
  - 测试替身现在写入 ignored 类别（`~/.cache/ProbeDemo`），让 verify 有机会给出 `verified`
    而不是永远 `no_writes`（§6.5：空集不算通过）。
- 2026-11-07 **第二十四轮：strace 降级落地**（没有 fuse 的机器不再直接放弃）：
  - `Probe` 的降级判据：`mounter.Available()` 失败 + `strace` 存在 → 走 strace；两者都没有才报 3。
  - `StraceCommand` 把 **bwrap 包在 strace 外层**：日志由宿主写，不用考虑"沙箱里往哪写日志"；
    不罩 overlay（HOME 就是真实的），也就没有"公共目录绑回去"这回事。
  - `ParseStrace` 的取舍：只认带写入意图的 open/mkdir/rename/link/truncate；`O_RDONLY`/`O_PATH`/`stat` 跳过；
    **返回 -1 的调用跳过**（`mkdir` 的 EEXIST 是第二次启动的常态）；`unlink` 记 whiteout；
    `<unfinished>`/`resumed` 拼接行直接丢。
  - 断言：读操作被过滤、失败调用被跳过、whiteout 单列、命令形状、降级链路产出可建实例的档案。
  - **未用真实日志校准**：本沙箱 `ptrace` 被禁（`PTRACE_TRACEME: Operation not permitted`），
    只能 fixture 校准；真实机器的 strace 输出若有格式差异，改 `straceLine` 正则即可。
- 2026-11-07 **第二十三轮：探测跑两轮**（§4 明确要求、之前只跑一轮）：
  - 登录态/token 常常第二次启动才落盘，跑一轮的典型结果是"探到配置目录、没探到凭据目录"。
    现在 `ProbeOptions.Rounds` 默认 2，两轮**共用同一份 overlay 写层**（结果是并集），
    每轮开始前打印「第 N/2 轮 —— 请打开应用用一下，然后把它关掉」。
  - 某轮取消 → `ErrCanceled`（不产出档案）；超时 → 4 并保留现场；其余失败 → 3。
    第一轮成功、第二轮失败的组合**不会**丢掉第一轮已观测到的写入（写层一直在）。
  - 断言：默认两次调用、提示出现两次、显式 `Rounds: 1` 只跑一次。
- 2026-11-07 **第二十二轮：`run --audit` 落地——§6.5 的生产者补上了**：
  - 审计层不是另写一个 argv 构造器，而是**插进 `BuildBwrapArgs` 的 ④ 与 ⑤ 之间**
    （`BuildOptions.Audit`）：overlay 罩住 HOME → 公共目录/AC_BASE 绑回真实 → 实例私有目录。
    另写一套的话，runtime/dbus/锁文件那部分迟早与运行期分叉，表现是"审计期能跑、平时跑不起来"。
  - `core/audit.go`：写层路径**确定性**（`/var/tmp/appclone/audit.<uid>/<app>.<id>`）——
    verify 必须自己能找到证据，随机目录名只能靠扫描猜；`run.lock` 已保证同一实例不会有两个会话。
  - `run.toml` 在**应用退出之后**才写：它是"这次审计正常跑完"的凭证，中途崩掉就不该留下它
    （那正是"证据不完整"的定义）。结束**保留**写层（不删），交给 verify 消费。
  - 上一轮还有未消费的证据（`quarantine-*` 或摘要写着 retained）→ **拒绝开始新审计**（3），
    否则会把可能含用户文件的现场覆盖掉。
  - 断言（不需要 fuse）：审计层顺序（公共目录/AC_BASE 在实例 bind 之前）、`run.toml` 只在结束后存在、
    完整生命周期（begin → finish → verify 消费）、pending 证据拒开。
- 2026-11-07 **第二十一轮：`doctor --verify` 开始按结论处置证据**（§6.5 剩下的另一半）：
  - 之前 verify 只判不落账：判完什么都不写，证据永远留在盘上，也没有 `summary.toml` 可重放。
    现在 `finalize`：`verified` → 写摘要 + 删 upper/run.toml（证据已消费）；`leak-detected` →
    upper 改名 `audit/quarantine-<时间>/`（**不删**，越界文件就在里面）+ 摘要记 `evidence_retained`；
    `extract_failed` → 现场原样保留；其余 unverified 不写摘要不动现场。
  - **顺序是"先搬/删、再写摘要"**：反过来的话，摘要一旦写成功而处置失败，下一次 verify 会按
    "已消费"重放一个错误结论，现场再也找不回。
  - CLI 在 leak 时直接打印 quarantine 路径并提示"先别删，把缺的文件取回来"。
  - 断言：verified 后 upper/run.toml 消失且重放结论一致；leak 后越界文件确实躺在 quarantine 里；
    extract_failed 时 upper 一动没动。三种处置都不需要 fuse，纯文件树验证。
- 2026-11-07 **第二十轮：L0 静态预填落地**（把"零执行"的三条线索接上）：
  - `core/static.go`：已知模式 / AppArmor `owner … w` / Flatpak `filesystems=` 三个渠道，
    产出低置信候选与**锁文件**。锁文件是重点：它让探测不再"零写入"——主实例在跑时应用会发现锁被
    占用、把参数转发过去然后退出（§3.2.2），而用户看到的是"这个应用不写盘"。现在 `add` 会先把
    锁文件 bind 成空文件再跑会话。
  - **召回如实记录**：本机 5 个 AppArmor profile 全是 `userns` 放行型（0 条 owner 规则）、
    没有 flatpak 应用，所以 L0 在这台机器上贡献为 0；中文系统靠的仍是探测（`user-dirs.dirs`
    解析 + overlay）。文档里"不要把 L0 描述成主力"这句话是对的。
  - 途中踩了一个自己的坑：AppArmor 规则 `owner <path>… <perms>,` 的**权限在最后一个字段**，
    我第一版按"每个 token 都得含 w"过滤，结果一条路径都解析不出来——单测（4 条规则覆盖通配/
    无 owner/只读/家目录外）当场抓到。
- 2026-11-07 **第十九轮：补齐会话内正向断言**（`discover.md` §3.2.1 唯一没落地的一条）：
  - 做法换了：契约原来写"写标记文件再在宿主读到"，那要往用户 `~/Downloads` 里写东西，
    中途被杀就留残留。改成**比对设备号**——overlay 是 fuse 设备，bind 生效的真实目录在底层
    文件系统上；公共目录若与 `$HOME` 同设备，说明 bind 没生效 → `exit 91` → 探测作废（4）。
    因为守卫跑在应用**之前**，这时还没有任何写入，可以安全删写层。
  - 公共目录经环境变量传（换行分隔，`My Documents` 这类带空格的名字不会被 word splitting 拆碎）。
  - **用真实 bwrap 验证过**（root 下、不需要 fuse）：有 bind → exit 0；去掉 bind → exit 91；
    设备号对照 81/81 vs 76（`/proc`）。这是探测里第一段真正跑过内核的断言。
  - 顺带修两处文案/用法：零写入的提示不再说"什么写入都没有"（只观测到缓存/共享时那句话是错的，
    实测数目会带出来）；`add <app> <id> --profile-only` 不再静默忽略 id。
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
  - `add --fork` 目录寿命写清：`root/<原路径>/` 是持久 upper 层，overlay 的 work/merged
    （现为 `.fork/<hash>/`，见第三十九轮）才可删——此前文档把它们混成"临时区"，照做会删掉用户数据。
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
  **已于第二十六轮补做**：只读 subagent 判 REJECTED（2 P1 + 10 P2），findings 与修复见上；
  **待复审**：修复后的工作树需再过一次只读审查才能把本轮标完成（重点：P1-1 清扫的锁窗口、
  P1-2 老凭证回退是否还有反向误判、`moveDir` 失败语义）。
