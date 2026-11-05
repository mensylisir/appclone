# discover：路径自动探测设计

> appclone 链路中最依赖人工的环节。本文定义"自动探测应用配置/数据目录"的完整方案
> 谱系、三层探测管线（v2 目标形态）与各层实现细节。
> 上游：[方案设计.md](方案设计.md) §5/§6.1（档案规范与 discover 命令）；
> 选型结论以 ADR-008 记入 [tech-stack.md](tech-stack.md)。

## 1. 问题定义

自动探测要同时满足三个属性，缺一不可：

| 属性 | 含义 | 不满足的后果 |
|---|---|---|
| **归因** | 得到的路径必须确属目标应用 | 混入其他进程噪声 → 隔离了不该隔离的目录 |
| **完备** | 应用全部状态目录都被捕获 | 漏一个 = 分身数据泄漏到主空间（功能性失败，非瑕疵） |
| **免 root** | 与项目红线一致 | 直接排除一半候选方案 |

另有一条软性约束：**探测期不能显著拖慢应用**——用户要在探测会话里正常使用应用
（收发文件、改设置），ptrace 级别的性能损耗会让 GUI 应用音视频卡顿，降低配合度。

## 2. 方案谱系

| # | 方案 | 归因 | 完备性 | 免 root | 评价 |
|---|---|---|---|---|---|
| 1 | **overlay-diff**（fuse-overlayfs + bwrap） | ✅ 物理（独占写层） | ✅ VFS 层全捕获 | ✅ | **主探测（推荐）** |
| 2 | **strace**（`-e trace=%file` + 写标志过滤） | ✅ 按 PID | ✅（限已行使路径） | ✅ | v1 现行方案，降级为兜底 |
| 3 | LD_PRELOAD 拦截 libc open/mkdir | ✅ | ⚠️ 静态链接、直接 syscall 漏掉 | ✅ | Chromium 沙箱绕过 libc，恰是重点目标 |
| 4 | inotify 递归监听 $HOME | ❌ 事件无 PID | ✅ | ✅ | 只在"全场仅目标应用在跑"时近似成立 |
| 5 | fanotify（带 PID 归因） | ✅ | ✅ | ❌ | 归因需 CAP_SYS_ADMIN，rootless 不可用 |
| 6 | eBPF 追 openat/mkdirat | ✅ | ✅ | ❌ | 发行版普遍 `unprivileged_bpf_disabled` |
| 7 | XDG 重定向探测（XDG_CONFIG_HOME 指向空目录再跑应用） | ✅ | ❌ 只对守规范应用成立 | ✅ | 硬编码 `~/.app` 的应用（重点难点）恰好漏掉 |
| 8 | 静态启发 / 知识库 | ✅（人工策展） | ❌ 低召回 | ✅ | 适合预填，不适合定案 |

## 3. overlay-diff：主探测原语

### 3.1 原理

把"探测"变成"差分"：给 `$HOME` 套一层 OverlayFS——
lowerdir = 真实 `$HOME`（只读），upperdir = 全新空目录。
应用在 bwrap 沙箱里把 overlay 的 merged 视图当 `$HOME` 正常运行；
会话结束后 upper 层里的每一条路径 = 该应用本次会话的写入集。

```
真实 $HOME (lower, ro) ──┐
                          ├── merged ← bwrap --bind 把它挂到 $HOME
空探测写层 (upper, rw) ───┘
                                ↓ 会话结束后
                    find upper → 写入路径集（零噪声）
```

### 3.2 参考实现（v2 core 的探测原语）

```bash
probe_run() {  # 用法: probe_run <可执行文件> [args...]
  local base="$AC_BASE/probe" upper="$AC_BASE/probe/upper" \
        work="$AC_BASE/probe/work" merged="$AC_BASE/probe/merged"
  rm -rf "$base" && mkdir -p "$upper" "$work" "$merged"

  fuse-overlayfs -o lowerdir="$HOME",upperdir="$upper",workdir="$work" "$merged"
  bwrap --dev-bind /dev /dev --proc /proc \
        --bind / / \
        --bind "$merged" "$HOME" \
        -- "$@"
  fusermount3 -u "$merged"

  # 写入目录集（排除 whiteout，目录即候选）
  find "$upper" -mindepth 1 -type d | sed "s|^$upper||"
  # 单例锁线索：whiteout = 创建后又被删除的瞬时文件（flock/socket 常见形态）
  find "$upper" -mindepth 1 -type c -exec stat -c '%n' {} \; | sed "s|^$upper||"
}
```

### 3.3 相对 strace 的决定性优势

1. **归因是物理性的**：写层里只有这一个应用在跑，`find` 出来的每条都是它的——
   不存在"syscall 过滤时漏判/误判"，探测逻辑无法写错；
2. **零开销、零漏网**：观测发生在 VFS 层，静态链接、直接 syscall
   （Chromium 沙箱路径）、Electron app.asar 内部逻辑全部逃不掉；
   ptrace 的 2~10 倍性能惩罚不存在，用户在探测会话里正常使用应用；
3. **免费的单例锁线索**：应用"启动时创建、退出时删除"的锁文件会在 upper 层留下
   whiteout（0,0 字符设备）。单独收集这些路径 → 高置信的 `lock_files` 候选；
   strace 里这类瞬时文件反而容易因运行时长不足而漏采。

### 3.4 已知坑与对策

| 坑 | 对策 |
|---|---|
| atime 更新引起伪 copy-up | 挂载时 `-o noatime`（探测场景 lower 本就该只读） |
| 只覆盖 `$HOME`，应用写 `/var`、`/opt` 不可见 | 与静态预填（§4.1）互补：可疑系统路径单独 overlay 或提示进 `extra_binds` |
| 只捕获"本次会话被行使过的"路径 | 流程弥补：GUI/CLI 明确提示"把功能尽量用一遍"；冷启 + 热启**双跑**（登录态目录只在第二次出现） |
| upper 出现大量 `~/.cache` 噪声 | 语义分类（§5）直接排除，不进候选 |
| fuse-overlayfs 不可用的环境 | 降级到 strace 路径（v1 现行逻辑） |

## 4. 三层探测管线（v2 目标形态）

```
L0 静态预填（零执行，秒级）
   ├─ 已知模式匹配：~/.config/<id>、~/.local/share/<id>、~/.<id>、~/.var/app/<id>
   ├─ AppArmor profile：/etc/apparmor.d/<binary> 的 owner-write 规则
   │   （发行版人工策展的写入路径清单，免费知识库）
   └─ Flatpak 元数据：flatpak info --show-permissions 的 persist 列表
        ↓ 低置信候选
L1 overlay-diff 主探测（动态定案）
   └─ §3.2 原语，产出高置信写入集 + whiteout 锁线索
        ↓
L2 语义分类（喂给 GUI 屏3 预勾选）
   └─ §5 规则表 → 绝大多数应用到屏3 时只剩"确认预勾选项"
```

层间关系：L0 不执行任何代码，负责把"冷启动前就能猜到的"先填上；L1 定案；
L2 把定案结果翻译成档案语义。**strace 降级为 L1 不可用时的兜底路径**
（v1 现行实现继续保留）。

## 5. 语义分类规则（L2）

| 匹配规则 | 分类 | 默认勾选 | 依据 |
|---|---|---|---|
| `~/.config/*`、`~/.local/share/*`、`~/.var/app/*` | 配置/数据 | ✅ 隔离 | XDG 规范目录，天然属于应用自身状态 |
| 路径含 `cache` / `.cache` / `Cache`（大小写不敏感） | 缓存 | ❌ 不隔离 | 丢失只影响首启速度；隔离则双份缓存 |
| XDG 公共目录（Downloads/Documents/Pictures/Music/Videos/Public/Templates） | 公共 | ❌ 强制排除 | 项目核心语义：公共文件必须共享（见方案设计 §1） |
| `~/.<name>` 顶层点目录 | 灰区 | ⚠️ 人工确认 | 硬编码路径的重点难点，交给屏3 勾选 |
| `/var`、`/opt`、`/etc` 下路径 | 系统级 | ⚠️ 进 `extra_binds` 提示 | 方案设计 §9：写 /etc 的应用不建议分身 |
| SQLite/LevelDB/大二进制文件所在目录 | 数据 | ✅ 隔离 | 文件形态启发（.db/.sqlite/*.ldb） |

## 6. 与 CLI / GUI 的映射

- L0+L1 产出 → 档案字段自动预填（`paths` 候选、`lock_files` 候选）；
- L2 分类 → GUI 屏3（discover 勾选对话框，见 `design/appclone-gui.html`）的
  chip 预判与勾选状态——管线跑完后用户面对的是"确认 2~3 个预勾选项"，
  而非"从 30 条 strace 输出里挑路径"；
- 探测会话提示语（CLI `discover` 与 GUI 对话框共用）：
  "在接下来的应用里把功能尽量用一遍——收发文件、截图、改设置——然后退出。"

## 7. 局限与后续方向

- **无法捕获未行使的路径**：探测是"本会话发生过什么"，不是"应用全部能力"——
  这是一切动态方案的共同上界，靠双跑 + 用户配合逼近；
- **档案库仍需版本演进**：应用升级改路径时需重跑（`appclone doctor` 的
  隔离校验可报警：分身运行时在实例目录外产生新写入 → 提示重探测）；
- 后续可加 **运行时审计模式**：分身本就运行在 bwrap 里，可长期开启
  "探测即运行"（overlay upper 即实例 root 的 `add --fork` 路线，见方案设计 §8.1），
  把一次性探测变成持续审计——这是 overlay 路线独有的演进方向，strace 给不了。
