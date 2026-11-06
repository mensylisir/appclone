# STATUS

> 只按测试证据更新。四态：`planned` 只有计划；`approved` 方案已过审查未实现；
> `implemented` 代码跑通验收命令但未过实现审查；`verified` 过审查且有实测证据（须附命令、日期、结果、覆盖范围）。

| 能力 | 状态 |
|---|---|
| 隔离语义（bwrap 绑定：写入落实例目录、宿主零污染、公共目录共享可见） | `implemented`（bash 原型期验证，原始输出未归档） |
| GUI 三屏设计稿交互（`design/appclone-gui.html`） | `verified` — 2026-11-06 `node design/guitest.js` → **58 通过 / 0 失败 / 无页面 JS 错误 / 退出码 0**（改稿后连续复跑 3 次一致）。覆盖三屏切换、探测**四态**（probing / confirm / **creating** / error，含"零写入 → 失败态不生成空档案"、"失败态重试回到探测中"、"creating 期间重复提交仍只建一个实例"）、**五个入口标题**（首次配置 / **重新配置（档案失效）** / 开启分身 / 新建分身 / 重新探测）**与四个按钮文案**（创建分身 / 开启分身 / 保存档案；reconfig 与 setup 同为「创建分身」，未单独断言），含"重新探测只更新档案不建实例"与"creating 期间 Esc/取消/点遮罩都关不掉"、"重命名撞名被拒"、"有档案可创建的应用→新建分身确认可用"、"候选行显示 N 个写入条目"、空态不藏列表且开关可达、共享但可写只读组（含代价文案）、探测中计数措辞（写入条目 ≠ 命中次数）、闸门 `app_running` 独立文案且不启动会话、降级横幅在探测页常驻、破坏性对话框默认响应为「取消」（回车不删）、探测中重入屏3 后过期回调不改写 tab 高亮、公共目录只读组、门禁只数「应用自身数据」（只勾缓存仍禁用）、搜索过滤、两处破坏性确认（删实例 / 关开关，含"取消后回弹"与"运行中先拒绝"）、运行中实例启动与删除双禁用、致命横幅禁用开关且环境恢复后双向可达、首次配置落详情页、Toast 与重命名（32 rune 上限）；**仅 Chromium 模拟，不含 GTK 渲染**。断言有牙由 7 组变异测试证明（默认响应改回破坏性按钮 / 空态藏整页 / creating 不禁用 / creating 关闭保护失效 / 去掉撞名检查 / 去掉写入条目渲染 / 删掉 telegram 候选，均立即变红） |
| 探测期隔离契约（公共目录摘出 / 单例强制隔离 / 进程组生命周期） | `approved`（`docs/discover.md` §3.2，Go 未实现） |
| 运行期审计与 `doctor --verify` 口径（`unverified` 不算通过） | `approved`（`docs/方案设计.md` §6.5） |
| 状态一致性（`AC_BASE/.lock`、原子写、`Mkdir` 判重）与运行态判定（`run.lock`） | `approved`（`docs/方案设计.md` §4.2/§4.3） |
| 实例目录布局、档案规范、`instance.toml`、CLI 命令面 | `approved`（`docs/方案设计.md` §4/§5/§6） |
| 桌面入口 `.desktop` 字段规范（绝对路径 `Exec` + `TryExec`、`display_name`） | `approved`（`docs/方案设计.md` §4.1） |
| GUI 选型（GTK4+libadwaita / puregotk）与三屏控件映射 | `approved`（`docs/tech-stack.md` §4，**S0 验收未做**） |
| 文档一致性（七轮交叉审查） | `verified` — 2026-11-06 七轮只读审查，findings 14 + 12 + 6 + 10 + 5 + 6 + 15（机制缺口）+ 16（第七轮复审：**1 P0 + 9 P1 + 6 P2**）共 **84 条**，逐条 grep 复核后修复。**第七轮的后台 subagent 被中断，改用前台只读审查完成**（同 AGENTS.md 要求，非静默跳过）。第八轮（5 个真实缺陷：脚本暴露 4 条 + 交叉审查 1 条）另记于 `NEXT.md`，不计入这 84 条：那 5 条是**设计稿行为 bug**，不是文档口径问题 |
| 探测 L1 overlay-diff、strace 降级 | `approved`（`docs/discover.md`；两者在 Go 中均未实现） |
| 探测 L0 静态预填、L2 语义分类 | `planned` |
| `core.BuildBwrapArgs`、实例 CRUD、克隆起步、GUI 二进制 | `planned` |
