# discover：路径自动探测设计

> appclone 链路中最依赖人工的环节。本文定义"自动探测应用配置/数据目录"的完整方案
> 谱系、三层探测管线（目标形态）与各层实现细节。
> 上游：[方案设计.md](方案设计.md) §5（档案规范）与 §6.1（`add` 的探测阶段；**没有独立的 `discover` 子命令**）；
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
| 2 | **strace**（`-e trace=%file` + 写标志过滤） | ✅ 按 PID | ✅（限已行使路径） | ✅ | 降级兜底（**尚未实现**；`legacy/` 脚本可作行为参照，不是产品代码） |
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

### 3.2 参考实现（`core` 的探测原语）

**前置检查（必须先跑，缺一项就走 §3.4 降级，不要硬试）**：

```bash
probe_available() {
  [ -r /dev/fuse ] && command -v fuse-overlayfs >/dev/null && command -v fusermount3 >/dev/null
}
```

**检查通过也不够**：静态检查（`/dev/fuse` 可读 + 命令存在）在容器、权限受限、上一轮 stale 挂载的场景都会给出假阳性，
真正挂载时才失败，而那时用户已经在探测页等着了。所以四道闸门之外再补一步：
在临时目录做一次 **`fuse-overlayfs` mount → unmount 的冒烟**（3 秒超时），结果进程内缓存一次；
失败按 §3.4 降级或退出 3，不要等用户点完开关才报错。

**写层位置是硬约束（不是布局偏好）**：`upper/`、`work/`、`merged/` 必须整体位于 lower（有效 `HOME`）**之外**。
实测（2026-11-06）：upper 嵌在 lower 里时 overlay **挂载成功**，但视图立刻递归——
`ls merged/<更深层路径>` 直接 `ELOOP`，`rm -rf` 也删不掉。默认 `AC_BASE` 就在 `$HOME` 里，所以
探测写层放 `/var/tmp/appclone/probe.<uid>.<rand>/`（`MkdirTemp`，0700），**不放 `$AC_BASE/probe`**（方案设计 §4.4）。

挂载前断言（缺一不挂）：
1. `realpath(upper)` 不是 `realpath(lower)` 的前缀（父子都不行）；
2. `workdir` 与 `upperdir` 同一文件系统（跨 fs 直接 EINVAL）；
3. `upper`/`work`/`merged` 三个目录都为空——上一轮残留先清，清不掉报环境错误，不要硬挂。

实测环境差异很大：本机（Arch）有 `fusermount3` 但**没有 `/dev/fuse`、也没装 `fuse-overlayfs`**，
所以主探测在当前机器上直接不可用——这类判断必须在运行期做，不能写在文档里当"应该都有"。

**探测会话与运行期会话是两套参数，不是一套**。四处差异，每一处都有对应的失败形态：

| 差异 | 探测期 | 运行期 | 缺了会怎样 |
|---|---|---|---|
| `$HOME` 的来源 | overlay 的 `merged`（写层 = `probe/upper`） | 真实 `$HOME`（靠 `--bind / /` 透传） | 探测不到写入 |
| **公共目录与工具自身目录** | **在 overlay 之上再 bind 回真实目录（rw）**，见下"公共目录摘出" | 不单独 bind | **探测结束删 upper 时连带删掉用户刚存的文件** |
| 单例机制 | **一律隔离**（私有 runtime + 私有 dbus + 空锁文件） | 按档案 `isolate_runtime`/`isolate_dbus` | 主实例在跑时应用把参数转发给主实例后退出，探测零写入 |
| 进程生命周期 | `setsid` 起独立会话 + 退出时整组 kill | 同 | 应用 daemonize 出的进程逃出 `--die-with-parent`，残留孤儿（实测 bwrap 0.13.1） |

#### 3.2.1 公共目录摘出（探测期第一红线）

overlay 把整个 `$HOME` 罩住，**用户在探测会话里的每一次保存都写进 upper**。会话结束删 upper，
`~/Downloads` 里刚存的附件就没了——这不是"探测不准"，是**删用户数据**，比隔离失效严重得多。

所以在 `--bind "$merged" "$HOME"` **之后**，逐个把下列路径 bind 回**真实**目录（源取宿主绝对路径，不是 merged 下的路径）：

```bash
# 顺序：overlay 先罩住 $HOME，再用真实目录把它盖回来（后绑定覆盖先绑定）
--bind "$REAL_HOME/Downloads"  "$HOME/Downloads"
--bind "$REAL_HOME/Documents"  "$HOME/Documents"
...                                    # 其余 XDG 公共目录同理
--bind "$AC_BASE"              "$AC_BASE"        # 工具自身状态目录，否则探测期写的 ui.toml 会被删
--bind "$REAL_HOME/.local/share/recently-used.xbel" ...  # shared_writes：隔离会让输入法/书签在探测会话里失效
```

- 公共目录清单**不在代码里写死，也不能靠猜**：解析 `~/.config/user-dirs.dirs`（`XDG_DOWNLOAD_DIR` 等键，
  不依赖 `xdg-user-dirs` 二进制——它在最小安装里很可能不存在），并逐项校验：
  **必须是绝对路径、必须落在有效 `HOME` 之下（含等号）、`basename` 不为空且不等于 `HOME`**。
  校验不通过（典型：`xdg-user-dir` 在键未配置时返回 `$HOME` 本身）就**禁用探测并报配置错误**，
  **不要回退到八个英文默认名**——回退在中文系统上会漏掉 `~/下载`，用户存的文件照样落进 upper 被删；
- **正向断言与反向断言成对**：反向断言只能证明"upper 里没有公共目录"，证明不了"bind 真的生效"。
  所以探测会话内还要对每个公共目录确认看到的是真实目录（`findmnt -no SOURCE --target` 指向真实设备/路径，
  或写一个标记文件再在宿主读到）。bwrap 的 bind 源按宿主根解析（实测 0.13.1），摘出"应该"生效——
  但"应该"不是证据，一条断言的成本远低于一次删用户文件；
- 这些路径**一律不进候选**：L2 规则表第 1 条已规定强制排除，这里是**运行期物理保证**，不是靠分类兜；
- **反向断言（必须实现）**：探测结束后检查 `upper` 下是否出现上述任一公共目录路径。
  出现即说明摘出失效（或应用绕过了 bind），**本次探测结果整体作废、丢弃候选并报错**，
  因为无法排除已有用户文件被写进 upper。宁可让用户重跑一次，也不能带着"可能删过文件"的探测结果去建实例；
- **作废时一律不删 upper**：把它改名成 `quarantine-<时间戳>/`（同一 `/var/tmp` 目录下），把恢复路径打进错误信息
  （`probe.method` 记 `quarantined`）。原文"§4 清理约定仍执行"与这条直接冲突——清理约定在这里让位于
  "不删可能属于用户的文件"，这也是审计层（方案设计 §6.5）的同一条规则，不许两处两种做法。

#### 3.2.2 探测期必须隔离单例

主实例正在运行时，应用启动后会通过 socket/DBus/flock 发现已有实例，把参数转发过去然后自己退出——
**探测会话里零写入，探测结果为"未观测到写入"**。这是最常见的探测失败，且表现和"应用真的不写盘"无法区分。

因此探测期**不看档案**（此时档案还不存在），固定全套隔离：

1. `--unshare-pid --unshare-ipc`；
2. 私有 `XDG_RUNTIME_DIR`（`probe/runtime`），wayland / pulse / session bus socket 逐个 bind 进去，
   否则探测会话里应用没有图形界面；
3. 私有 DBus session bus（临时 `dbus-daemon --session --nofork --print-address`），
   地址经 `--setenv DBUS_SESSION_BUS_ADDRESS` 注入；
4. L0 静态预填补到的锁文件路径，bind 一个空文件盖住（**仅普通文件**，socket 一律走第 2 条的私有 runtime）。

**探测开始前还要挡一道**：扫描目标应用是否已有进程在跑（按 `Exec` 的 basename 比对 `/proc/*/comm`，
并用 `/proc/<pid>/ns/mnt` 排除掉已在本工具 namespace 里的分身进程）。在跑就让 GUI 提示
「请先把「微信」完全退出，再开始跟踪」并禁用「开始」——**不要求用户自己去猜为什么没反应**，
也不要在应用还开着的时候硬跑一轮然后报"未观测到写入"。

#### 3.2.3 生命周期：别信 `--die-with-parent`

实测 bwrap 0.13.1：应用自己 `fork` + `setsid` 出来的子进程不继承父进程的 death signal，
`--die-with-parent` 只管住直接子进程。探测期残留的进程会继续写 upper、并在清理后仍持有 overlay 挂载。

- 探测进程用 `setsid` 起独立会话，记录 pgid；**必须断言 `Getpgid(child.Pid) == child.Pid` 才允许负 pgid kill**——
  setsid 失败或读早了会拿到调用者自己的 pgid，`kill(-pgid)` 就把终端/GUI 整个会话杀掉（灾难级误杀）。
  断言不过就只 `kill(pid)` 并报错，绝不退化成组杀；
- 用户取消 / 超时 / 正常结束，一律 `kill(-pgid, SIGTERM)`，3 秒后仍在则 `SIGKILL`；
- **组杀管不到的东西要单独写清**：应用自己 `setsid` 出来的进程已换进程组，打不到；它若还在 PID namespace 内，
  靠 `--unshare-pid` 的 PID 1 退出连带清空；若被宿主的 systemd/DBus 激活拉起（连 namespace 都不在），
  两者都收不到。所以清理分三层写：**pgid（bwrap 与未 setsid 的后代）→ namespace teardown（沙箱内全部）→
  宿主导出的激活进程（需要按 `/proc/<pid>/ns/mnt` 扫描，命中就提示"有进程逃出沙箱"）**。
  "整组 kill 解决 daemonize"是过度承诺，不能这么写；
- **`fuse-overlayfs` 进程要由 core 自己起自己收**（`SysProcAttr{Pdeathsig: SIGKILL}` + `auto_unmount` 挂载选项），
  否则 core 被 SIGKILL 后挂载点变成 `Transport endpoint is not connected`，下次挂载直接 EBUSY；
- **启动前必须扫残留**：`/var/tmp/appclone/probe.*` 下属于本 uid 的目录 → 读 `/proc/self/mountinfo` 判断是否仍挂载
  → 是则 `fusermount3 -uz` → **删前先做污染检查**（可能含用户文件）→ 校验通过才删；
  残留清理失败就报环境错误（3），不许"清理不了就继续跑"；
- **探测期的 `dbus-daemon` 要单独回收**：它是 `core` 起的兄弟子进程，不在 bwrap 那个 session/pgid 里，
  `kill(-pgid)` 打不到它，删 `probe/` 也只删掉 socket 文件、进程仍挂在用户会话上。
  必须显式 kill 并等它退出（与运行期共用 §6.6 的同一份生命周期实现，不写两套）；
- 再 `fusermount3 -u`，最后删 `probe/`；
- 顺序不能反：先杀进程再卸载，否则卸载会 EBUSY。

**清理必须在收集与检查之后**：先收集 upper 里的写入集、再做"公共目录污染"检查（§3.2.1 的反向断言），
最后才删 `probe/`。反过来（先删再查）这个断言永远为空——它检查的东西已经被自己删掉了。

Go 实现用 `os/exec` + `SysProcAttr{Setsid: true}` + `context`，**不依赖 shell 的 `trap`**
（`EXIT` trap 里引用 `local` 变量会失效，这正是上一版参考实现踩过的坑）。

#### 3.2.4 参数序列（语义参考）

```bash
probe_run() {  # 参考语义；Go 实现用 os/exec + context + Setsid，不用 shell trap
  setsid bwrap --unshare-user --unshare-pid --unshare-ipc \
        --bind / / --dev-bind /dev /dev --proc /proc \
        --bind "$merged" "$EFFECTIVE_HOME" \                # 有效 HOME（AC_HOME 或真实 HOME）
        --bind "$EFFECTIVE_HOME/Downloads" "$EFFECTIVE_HOME/Downloads" \  # 其余公共目录同理，必须在其后
        --bind "$AC_BASE" "$AC_BASE" \                       # 工具状态根（但要排除 probe/ 与 instances/）
        --bind-try "$SW" "$SW" \                             # shared_writes，不存在就跳过
        --bind "$probe_runtime" "$XDG_RUNTIME_DIR" \          # 私有 runtime
        --ro-bind-try "$HOST/wayland-0" "$probe_runtime/wayland-0" \  # 宿主 socket 只读逐个引入
        --setenv XDG_RUNTIME_DIR "$probe_runtime" \
        --setenv DBUS_SESSION_BUS_ADDRESS "$probe_bus" \      # = dbus-daemon --print-address 的原文
        --setenv HOME "$EFFECTIVE_HOME" \                     # AC_HOME 生效时不能漏（方案设计 §6.4）
        --die-with-parent \                                   # 兜底；主保证是 pgid kill
        -- "$@"
  # 结束后：kill(-pgid) → kill 并 wait 探测期 dbus-daemon → kill fuse-overlayfs → fusermount3 -u
  #        （失败退 -uz）→ 收集 upper → 正/反向断言（摘出是否生效、有没有公共目录污染）
  #        → 通过才删 probe/；命中则 quarantine 保留现场
  # 顺序与 §3.2.3 一致；dbus-daemon 与 fuse-overlayfs 不在 bwrap 的 pgid 里，必须单独收
}

# 写入集收集：不能只 find -type d（会漏掉 $HOME 下的顶层单文件，如 ~/.apprc），
# 也不能把 ~/.config 这类纯父目录当候选（用户勾中它 = 隔离所有应用的配置）。
collect_upper() {
  local up="$1"
  # 1) 普通条目（目录与单文件都算）
  find "$up" -mindepth 1 ! -name '.wh.*' -printf '%y %P\n'
  # 2) whiteout 三形态都识别（rootless 下未必是字符设备）
  #    a. overlayfs 字符设备 0,0   b. .wh. 前缀命名   c. user.fuseoverlayfs.whiteout xattr
  find "$up" -mindepth 1 -type c -printf '%P\n'
  find "$up" -mindepth 1 -name '.wh.*' -printf '%P\n'
  find "$up" -mindepth 1 -type f -exec getfattr -n user.fuseoverlayfs.whiteout --only-values {} \; -print 2>/dev/null
}
# 归并规则：取"最深的应用目录"或"单文件本身"，剔除 XDG 根（~/.config、~/.local/share、~/.local/state）
# 及其所有祖先；再交给 L2 分类。判"零写入"看的是归并后的候选数，不是 upper 条目数。
```

**顺序是契约**：`--bind / /` 必须在 `--dev-bind /dev /dev`、`--proc /proc` **之前**——
bwrap 对同一目标后挂载覆盖先挂载，写反了前两条会被整体抹掉（探测期看不出问题，
运行时就是"看见的是宿主 /dev 和宿主 /proc"，隔离语义已经错了）。
同理，**公共目录的 bind 必须排在 `--bind "$merged" "$HOME"` 之后**，排前面等于被 overlay 罩回去，
用户文件照样落进 upper。同一套顺序规则见[方案设计.md](方案设计.md) §3.1。

`--unshare-uts` **不在参数集里**：它不带来任何隔离收益（hostname 副本本来就与宿主一致），
却让 namespace 内的 hostname 与共享的 `/etc/hostname` 存在不一致的可能，
部分应用（Java `InetAddress.getLocalHost()`、hostname 自检类）会因此走异常分支。
少一个 namespace 少一个变量，理由记在 [tech-stack.md](tech-stack.md) ADR-021。

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
| **探测期用户存的文件随 upper 被删** | §3.2.1 公共目录摘出 + 结束后反向检查 upper 污染；命中即作废本次结果 |
| **主实例在跑导致零写入** | §3.2.2 探测前扫进程拦住 + 探测期强制隔离单例（不看档案） |
| **应用 daemonize 出孤儿进程** | §3.2.3 `setsid` + 整组 kill；`--die-with-parent` 只当兜底，不当保证 |
| 探测结束仍挂载着 overlay（`EBUSY`） | 先杀进程组再 `fusermount3 -u`，顺序不能反 |
| 只覆盖 `$HOME`，应用写 `/var`、`/opt` 不可见 | 与静态预填（§4）互补：可疑系统路径单独 overlay 或提示进 `extra_binds` |
| 只捕获"本次会话被行使过的"路径 | 流程弥补：GUI/CLI 明确提示"把功能尽量用一遍"；冷启 + 热启**双跑**（登录态目录只在第二次出现） |
| upper 出现大量 `~/.cache` 噪声 | 语义分类（§5）直接排除，不进候选 |
| fuse-overlayfs 不可用的环境（无 `/dev/fuse`、未装包、容器内） | 按 §3.2 前置检查判定后**降级到 strace 路径**（计划中的分支，`legacy/` 原型可参照），并把 `probe_method = "strace"` 写进档案——证据强度不同，不能混标为同一来源 |

**降级判据（三选一即降级，不重试、不猜）**：`/dev/fuse` 不可读；`fuse-overlayfs` 不在 PATH；
`fusermount3` 缺失。降级时 CLI/GUI 必须说明"本次用 strace 探测，覆盖率更低、需要你多操作一会儿"，
不得静默换方法。

## 4. 三层探测管线（目标形态）

```
L0 静态预填（零执行，秒级）
   ├─ 已知模式匹配：~/.config/<id>、~/.local/share/<id>、~/.<id>、~/.var/app/<id>
   ├─ AppArmor profile：/etc/apparmor.d/<binary> 的 owner-write 规则
   │   （发行版人工策展的写入路径清单，免费知识库——**但仅当该发行版启用 AppArmor；
   │    Arch/EndeavourOS 默认不启用，/etc/apparmor.d 通常不存在，此时本项静默跳过**）
   └─ Flatpak 元数据：flatpak info --show-permissions 的 persist 列表
        （仅对 flatpak 应用有效，非 flatpak 应用静默跳过）
        ↓ 低置信候选
L1 overlay-diff 主探测（动态定案）
   └─ §3.2 原语，产出高置信写入集 + whiteout 锁线索
      （前置检查不过则整层替换为 strace 分支，档案记 probe_method）
        ↓
L2 语义分类（喂给 GUI 屏3 预勾选）
   └─ §5 规则表 → 绝大多数应用到屏3 时只剩"确认预勾选项"
```

层间关系：L0 不执行任何代码，负责把"冷启动前就能猜到的"先填上；L1 定案；
L2 把定案结果翻译成档案语义。**strace 降级为 L1 不可用时的兜底路径**
（该分支尚未在 Go 中实现，`legacy/` 原型仅作行为参照）。

**L0 的真实召回预期**：Arch 上 AppArmor 与 Flatpak 两项大概率都拿不到，
L0 实际只剩"`~/.config/<id>` 模式匹配"——对守 XDG 规范的应用够用，
对写 `~/.<app>` 的老式应用召回为零（那正是 L1 存在的理由）。**不要把 L0 描述成主力**。

**探测会话怎么跑**：登录态目录往往只在**第二次启动**才出现（首启写配置、二次写 token），
所以一次"开关"动作内部必须跑**两轮**：冷启一轮 → 用户操作 → 退出 → 再启动一轮 → 退出。
GUI 只对用户说"打开应用用一下，然后关掉"，两轮循环由工具自己安排（这是"复杂度不泄漏给用户"的具体落点）。

**开始探测前的四道闸门（顺序执行，任一不过就不启动探测会话）**：

| # | 闸门 | 不过时的行为 |
|---|---|---|
| 1 | bwrap 可用、user namespace 可建 | 报环境不可用（方案设计 §6.3，退出码 3），不进探测 |
| 2 | fuse 三件套可用（§3.2 前置检查） | 降级 strace，并在界面显式说明覆盖率更低 |
| 3 | 目标应用当前**没有**实例在跑 | 提示「请先把「X」完全退出」并禁用开始按钮；**不给"仍然继续"**——继续只会得到零写入和一条假结论 |
| 4 | 拿到 `AC_BASE` 的跨进程写锁（方案设计 §4.2） | 等锁，超时 5 秒报"另一个 appclone 正在操作"，不并发探测（两个探测共用一个 `probe/` 会互相污染） |

第 3 道闸门的判定只看"目标应用自身"的进程：已在本工具 namespace 里运行的分身进程要排除
（否则用户开了分身就永远无法探测主实例）。判定方法见 §3.2.2。

## 5. 语义分类规则（L2）

| 匹配规则 | 分类 | 默认勾选 | 依据 |
|---|---|---|---|
| `~/.config/*`、`~/.local/share/*`、`~/.var/app/*` | 配置/数据 | ✅ 隔离（**见下方例外**） | XDG 规范目录，天然属于应用自身状态 |
| 路径含 `cache` / `.cache` / `Cache`（大小写不敏感） | 缓存 | ❌ 不隔离 | 丢失只影响首启速度；隔离则双份缓存 |
| XDG 公共目录（**Desktop**/Downloads/Documents/Pictures/Music/Videos/Public/Templates，共 8 项，实际清单解析 `~/.config/user-dirs.dirs`） | 公共 | ❌ 强制排除 | 项目核心语义：公共文件必须共享（见方案设计 §1）；进入 `paths` 视为档案非法 |
| 共享可写文件（`recently-used.xbel`、gtk bookmarks、输入法状态） | 共享可写 | ❌ 不隔离，进 `shared_writes` | 隔离后功能会坏（输入法失效、书签消失）；两实例互相追加是可接受代价，但要在界面说明（方案设计 §5.4） |
| keyring / Secret Service（凭证） | **不可隔离** | ❌ 标 `unsupported` | 凭证存在 DBus 服务里，不在文件路径上——**不进 `shared_writes`**（那是"可共享但可写"的白名单，放进去会让 `doctor --verify` 对账口径失效）；见方案设计 §5.4/§9 |
| `~/.<name>` 顶层点目录 | 灰区 | ⚠️ 人工确认 | 硬编码路径的重点难点，交给屏3 勾选 |
| `/var`、`/opt`、`/etc` 下路径 | 系统级 | ⚠️ 进 `extra_binds` 提示 | 方案设计 §9：写 /etc 的应用不建议分身 |
| SQLite/LevelDB/大二进制文件所在目录 | 数据 | ✅ 隔离 | 文件形态启发（.db/.sqlite/*.ldb） |

**规则优先级（从上往下匹配，先命中先定，不许两条规则同时生效）**：

1. **XDG 公共目录 → 强制排除**（最高优先级，任何规则不得翻案）；
2. **共享可写文件 → 进 `shared_writes`，不隔离**。这一条**必须压过** `~/.config/*` 的默认勾选，
   否则会踩到具体故障：`~/.config/fcitx5` 被隔离 → 分身里输入法直接失效；
   `~/.config/gtk-3.0/bookmarks` 被隔离 → 文件选择器里书签全没了。这两处是**硬排除**，不是"建议不隔离"；
3. `cache`/`Cache` 命中 → 不隔离；
4. `~/.config/*` 等 XDG 目录 → 默认勾选；
5. 顶层点目录、系统路径 → 灰区，交给屏3 人工确认。

实现上这份优先级就是一个有序规则表，`core` 里单一实现、可单测；**不要在 GUI 里再写一遍判断**。

**设计稿里有一处刻意的演示简化，别照抄**：`design/appclone-gui.html` 把 `~/.dingtalk`、`~/.mozilla` 这类
顶层点目录标成 `config` 且默认勾选，是为了演示"钩子长什么样"；**真实实现按第 5 条走灰区、默认不勾**。
照抄的后果与"误把公共目录隔离掉"同一级别：用户点一下确认，把应用写死在 `$HOME` 的目录当成应用数据，
这个目录里可能存着用户自己放的文件。

## 6. 与 CLI / GUI 的映射

- L0+L1 产出 → 档案字段自动预填（`paths` 候选、`lock_files` 候选）；
- L2 分类 → GUI 屏3（探测确认页，原生形态为 `AdwNavigationView` 的 push 页，见 `tech-stack.md` §4.5/§4.6）的
  chip 预判与勾选状态——管线跑完后用户面对的是"确认 2~3 个预勾选项"，
  而非"从 30 条 strace 输出里挑路径"；
- 屏3 是**四态**（`probing` / `confirm` / `creating` / `error`），不是"进去就有候选"
  （缺前两态就是"点完开关一片空白，用户以为坏了"；缺 `creating` 则连点两下建出两个实例）：

  | 态 | 界面 | 可做什么 |
  |---|---|---|
  | `probing` | `GtkSpinner` + "正在跟踪「X」的写入…" + **已发现 N 个写入条目**（流式；不是"命中次数"，overlay 拿不到次数）+ 常驻 `AdwBanner`（走 strace 时说明覆盖范围） | **取消**（必给，探测十秒到一分钟） |
  | `confirm` | **四组**：应用自身数据（可勾，每行带"N 个写入条目"）/ 单例处理（可勾，**由检出的单例项驱动，没检出就不出现**）/ **公共目录（强制共享，只读 `AdwActionRow` + `.dim-label`）** / **共享但可写（不隔离，只读，写明代价）**；走 strace 时**同一个 `AdwBanner` 从探测中起常驻，切到本态不消失** | 勾选、创建、取消 |
  | `creating` | "正在创建…"，确认与取消**都不可用**（事务已开始；再提交一次会建出两个实例，中途取消会留下"档案写了、实例没建成"的半成品） | 无（等它跑完） |
  | `error` | `AdwStatusPage`：**按 `Reason` 分四套文案**（`app_running` / `env_unavailable` / `no_writes` / `public_dir_leak`）+ 「重试」 | 重试、取消 |

  「公共目录」组即使全部不可勾也必须出现——它是"工具没有乱来"的唯一可见证据
  （方案设计 §6.1 第三条 UI 约束）。组内条目来自 §3.2.1 的摘出清单，与候选同一份数据源，不另写一份规则；
- 置信度必须**可见**，但只说**证据来源与覆盖范围**，不许承诺"完整"：
  `probe_method = overlay` 标"覆盖本次会话实际发生的写入（没走到的功能分支查不到）"，
  `strace` 标"覆盖率低（只记录实际跑到的路径，建议多操作一会儿再重跑）"。
  这不是提示语，是决策依据——用户据此决定要不要重跑；写成"结果完整"等于替他做了"不用再复核"的决定
  （方案设计 §6.1 文案红线）；
- 探测会话提示语（CLI 的 `add` 探测阶段与 GUI 屏3 共用；**没有独立的 `discover` 子命令**）：
  "在接下来的应用里把功能尽量用一遍——收发文件、截图、改设置——然后退出。"
- **零写入不是成功**：探测结束写入集为空时一律进 `error` 态（"这次没有跟踪到任何写入"），
  **不允许**退化成"生成一个空档案"——空档案等于分身与主实例完全共享数据，用户却以为隔离了。

## 7. 局限与后续方向

- **无法捕获未行使的路径**：探测是"本会话发生过什么"，不是"应用全部能力"——
  这是一切动态方案的共同上界，靠双跑 + 用户配合逼近；
- **档案库仍需版本演进**：应用升级改路径时需重跑。**开启审计**（`run --audit` 或档案 `audit = true`）时，
  `doctor --verify` 能发现"分身运行期在实例目录外产生新写入"并提示重探测（机制见[方案设计.md](方案设计.md) §6.5）；
  未开启审计时它只能输出 `unverified`，**不承诺常态报警**；
- 后续可加 **运行时审计模式**：分身本就运行在 bwrap 里，可长期开启
  "探测即运行"（overlay upper 即实例 root 的 `add --fork` 路线，见方案设计 §8.1），
  把一次性探测变成持续审计——这是 overlay 路线独有的演进方向，strace 给不了。
