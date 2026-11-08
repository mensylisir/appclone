# appclone · 应用分身

把 Android 的「应用分身 / Cloned Apps」搬到 Linux 桌面：**只隔离一个应用自己的配置与数据目录**，
让它以第二个身份运行；公共文件、D-Bus 会话、外设等其余一切照常共享。

不需要 root，不需要改应用本身，也不需要终端——普通用户的入口是图形界面里的一个开关。

```
主实例  ~/.config/微信        ← 两个实例互不可见
分身    ~/.local/share/appclone/instances/wechat/main/root/.config/微信
其他    ~/Downloads、~/Documents、桌面会话……   ← 照常共享
```

## 系统要求

| 依赖 | 说明 |
|---|---|
| glibc 系统 | CLI 是静态链接的 Go 程序；GUI 也零 cgo，但通过 `dlopen` 在运行期加载宿主 libc/GTK，因此要求 glibc；musl/Alpine 不在支持范围 |
| bubblewrap | 隔离引擎（必需） |
| GTK ≥ 4.14 + libadwaita ≥ 1.5 | 仅 GUI 需要（运行库，不需要开发包） |
| fuse-overlayfs | 推荐；缺失时路径探测降级为 `strace`，启动不受影响 |
| strace | 推荐（与 fuse-overlayfs 二选一即可探测）；两者都缺时不能首次探测新应用，已有合法档案仍可建普通分身（克隆起步会降级） |

Ubuntu 23.10+ 默认限制非特权 user namespace。deb / rpm / Arch 包的安装脚本都会在检测到该限制时安装
`/etc/apparmor.d/bwrap`（只放开 bwrap 自身，不改全局 sysctl）；手工安装 `.tar.gz` 时见「排障」。

## 安装

发行包（deb / rpm / Arch）与 `.tar.gz` 从 GitHub Releases 获取：

```bash
sudo apt install ./appclone_<version>_amd64.deb        # Ubuntu 24.04+ / Debian 13+
sudo dnf install ./appclone-<version>-1.aarch64.rpm    # Fedora 41+ / RHEL 10
sudo pacman -U appclone-<version>-1-x86_64.pkg.tar.zst
```

Arch Linux ARM 是独立项目，请使用 `.tar.gz`（本仓库只发 x86_64 的 Arch 包；AUR 源码包尚未提交，不要把它当现成安装方式）。

从源码构建（无需 C 工具链）：

```bash
CGO_ENABLED=0 go build -o appclone     ./cmd/appclone
CGO_ENABLED=0 go build -o appclone-gui ./cmd/appclone-gui
```

## 使用

打开「应用分身」，在「所有应用」里点亮某个应用的开关。第一次会先探测它把数据写到哪里，
确认后创建分身；之后启动、停止、重命名、删除都在详情页完成。所有删除都进回收站。

命令行是同一个 `core` 的开发者视图，命令面与 GUI 一一对应：

```bash
appclone add wechat work      # 探测（或复用档案）并创建实例
appclone run wechat work      # 启动分身
appclone stop wechat work     # 停止
appclone list                 # 实例与运行状态
appclone rm wechat work       # 移入回收站
appclone doctor               # 环境自检
appclone add wechat work2 --fork   # 从现有数据克隆起步（写时复制）
```

`--fork` / GUI 的「从现有应用数据克隆起步」不复制文件：运行期用 `fuse-overlayfs` 把主实例
数据作为只读底层，分身写过的内容落进自己的实例目录。**克隆是实时数据不是快照**——分身与主
实例不要同时使用同一份应用数据；需要冻结起点时先复制数据。

## 安全与边界

- 隔离范围只有档案 `paths` 里声明的目录；`--bind / /` 让其余路径全透传——公共目录的共享是
  产品语义，不是漏洞。
- 不需要 root、没有 daemon、没有 IPC、没有 webview；GUI 启动的分身由独立 runner 进程运行，
  关掉设置窗口不会结束它们。
- 删除实例、关闭应用开关都只把数据移入回收站（`gio trash`），永久删除由用户在回收站完成。
- 应用自己的数据属于用户：本项目不收集、不上传任何内容。

## 排障

```bash
appclone doctor
```

- **`bwrap: setting up uid map: Permission denied`（Ubuntu 23.10+）**：安装包应已处理；手工安装
  `.tar.gz` 时在解压目录执行
  `sudo cp bwrap.apparmor /etc/apparmor.d/bwrap && sudo apparmor_parser -r /etc/apparmor.d/bwrap`。
- **「缺 fuse-overlayfs」横幅**：安装 `fuse-overlayfs` 后重启 GUI；不装但装了 `strace` 也能用，探测覆盖不完整。两者都缺时 `appclone doctor` 会报 fail：已有合法档案仍可建普通分身，但无法首次探测新应用，克隆起步也会降级。
- **GUI 起不来**：确认 `libgtk-4.so.1` 与 `libadwaita-1.so.0` 的版本达到上表下限。

## 开发

```bash
gofmt -l . && go vet ./... && go test ./...       # Go 门禁
go vet -tags gtk_e2e ./cmd/appclone-gui          # GTK 测试文件的类型检查
staticcheck ./...                                 # CI 阻塞门禁（go install honnef.co/go/tools/cmd/staticcheck@v0.8.2）
CGO_ENABLED=0 go build ./...                      # 全仓零 cgo
node design/guitest.js                            # HTML 交互基线
design/guitest-gtk.sh                             # 真实 GTK 闸门（需显示 + bwrap）
```

设计契约在 `docs/方案设计.md`，技术选型与 ADR 在 `docs/tech-stack.md`，已验证状态在
`docs/STATUS.md`；`NEXT.md` 记录当前进度。打包配置在 `packaging/`。
