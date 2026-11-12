package core

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BuildBwrapArgs 是项目的事实核心（§3.1）。参数集与顺序都是契约：
// 缺一项或调错顺序就算实现错误，golden 断言覆盖 argv 全序而不是"包含某参数"。
//
// 纯函数：不读环境变量。需要探测的路径（BaseReal、HostBusPaths、RuntimeSocks）
// 由调用方在 BuildOptions 里给出——这里 stat 一次就会让"能不能构造参数"依赖
// 于运行时状态，doctor 也就无法离线展示将要执行的命令。
func BuildBwrapArgs(p Profile, in Instance, o BuildOptions) ([]string, error) {
	if err := ValidateProfile(p, o); err != nil {
		return nil, err
	}
	// 私有总线只有真的会生成 bind/setenv 时才算数。isolate_dbus 隐含
	// runtime 隔离；总线启动失败（DBusAddress 为空）时退回共享宿主总线，
	// 此时 runtime 目录仍然隔离，但宿主 bus 必须经 RuntimeSocks 绑回来。
	needRuntime := p.IsolateRuntime || (p.IsolateDBus && o.DBusAddress != "")
	// 校验 2（§3.1）：Base 不得等于或位于**tmpfs 之后**的任一 bind 目标之下，
	// 也不得包含它们。runtime 目录与每个 socket 都是 tmpfs 之后才绑的：
	// AC_BASE 若在 $XDG_RUNTIME_DIR/X11-unix 下，`--ro-bind-try` 会把整个
	// X11-unix 目录绑回来，Base 连同实例数据/keyring 又被应用看见。
	// 没有 runtime bind 时这些目标不存在，不能误拒（Base 落在 runtime 下是
	// 合法的临时目录用法）。
	if err := o.checkBaseVsLateBinds(needRuntime); err != nil {
		return nil, err
	}
	paths, err := normalizePaths(p.Paths, o)
	if err != nil {
		return nil, err
	}

	args := []string{
		"--unshare-user",                 // ① 非特权 user namespace；缺此项普通用户直接起不来
		"--unshare-pid", "--unshare-ipc", // ② 不共享 PID/IPC 视图
		"--bind", "/", "/", // ③ 全透传：公共目录共享的根
		"--dev-bind", "/dev", "/dev", // ④ 必须排在 ③ 之后，同目标后挂载覆盖先挂载
		"--proc", "/proc",
	}

	// ④.1 宿主会话总线遮蔽：`--bind / /` 会让 dbus-launch 之类放在 /tmp 的
	// 宿主总线在沙箱里仍然可见，应用可以绕过私有总线连回宿主（进而触达宿主
	// keyring）。只在私有总线**确实在跑**时遮蔽：总线启动失败走的是"降级为
	// 共享宿主总线"，那时遮蔽等于把应用彻底断网。
	// 用 `--bind` 而不是 `--bind-try`：清单已按"真实存在的 socket"过滤过
	// （core/dbus_session.go，含软链解析），-try 的容错没有意义；stat 与 bwrap
	// 之间 socket 消失的极小窗口可能留下 0444 占位文件，fail-closed 可接受。
	if p.IsolateDBus && o.DBusAddress != "" {
		for _, pth := range o.HostBusPaths {
			args = append(args, "--bind", "/dev/null", pth)
		}
	}

	// ④.5 审计层（§6.5）：只在 `run --audit` 时存在。顺序写死——
	//   overlay 先罩住 HOME，再把**公共目录**绑回真实位置，
	//   最后才是 ⑤ 的实例私有目录。反过来（实例 bind 在前）会被公共目录的
	//   bind 盖掉，应用数据反而落进 upper，verify 会把正常数据当越界。
	//
	//   AC_BASE 不再 bind 回真实位置：那样等于把实例目录（含其他分身的
	//   keyring/密码/私有总线）重新递给应用。审计期应用不需要 AC_BASE，
	//   run.toml/summary 由工具在沙箱外写。
	if o.Audit != nil {
		args = append(args, "--bind", o.Audit.Merged, o.Home)
		for _, pb := range o.Audit.PublicBinds {
			args = append(args, "--bind", pb.Src, pb.Dst)
		}
	}

	// ④.6 AC_BASE 遮蔽：--bind / / 会把工具自己的运行时根（其他分身的数据、
	// keyring、私有总线 socket）暴露给应用。tmpfs 只盖真实路径：bwrap 拒绝
	// 在软链目标上挂载（Can't mount on symlink destination），解析一次后软链
	// 自然被盖住。顺序必须在审计层之后、所有以 Base 为源的 bind 之前。
	if base := o.baseReal(); base != "" {
		args = append(args, "--tmpfs", base)
	}

	// ④.7 审计写层对应用不可见：分身 A 不该读到分身 B 的 upper 副本。
	// 写层根在 XDG_RUNTIME_DIR 内时随后的 runtime bind 会覆盖它，tmpfs 冗余
	// 但无害；golden 不断言该情形。
	if o.Audit != nil && o.Audit.WriteRoot != "" {
		args = append(args, "--tmpfs", o.Audit.WriteRoot)
	}

	// ⑤ 仅 paths 声明的目录，父先子后（normalizePaths 已给出全序）。
	for _, abs := range paths {
		src, err := instanceChild(in.Root, abs)
		if err != nil {
			return nil, err
		}
		// fork 实例：源是 overlay 的挂载点（lower=主实例数据、upper=实例 root）。
		// 只有挂载成功的路径才在表里；失败的路径退回普通实例目录，启动不中断。
		if merged, ok := o.ForkSources[abs]; ok && merged != "" {
			src = merged
		}
		args = append(args, "--bind", src, abs)
	}

	// ⑥ shared_writes 必须排在 ⑤ 之后：paths 可能声明了父目录（如 ~/.config），
	// 共享项要在它之上把真实文件盖回来，否则分身里输入法失效、书签消失（§5.4）。
	// 源不存在时用 -try 跳过，绝不代建（§3.2 第 3 条）。
	shared := make([]string, 0, len(p.SharedWrites))
	for _, s := range p.SharedWrites {
		abs, err := o.expand(s)
		if err != nil {
			return nil, err
		}
		shared = append(shared, abs)
	}
	sort.Slice(shared, func(i, j int) bool { return pathLess(shared[i], shared[j]) })
	for _, abs := range shared {
		args = append(args, "--bind-try", abs, abs)
	}

	// ⑦ extra_binds 排在 ⑤⑥ 之后：它是"档案里没有的补充"，盖在实例层之上才符合意图。
	for _, b := range p.ExtraBinds {
		src, err := o.expand(b.Src)
		if err != nil {
			return nil, err
		}
		dst, err := o.expand(b.Dst)
		if err != nil {
			return nil, err
		}
		args = append(args, "--bind", src, dst)
	}

	// ⑧ 普通文件锁：挂一个 inode 不同的私有空文件，flock 自然不再互斥。
	// 文件名用目标路径的哈希而不是数组下标：档案顺序一变私有锁就换名，旧文件永久残留。
	locks := make([]string, 0, len(p.LockFiles))
	for _, lf := range p.LockFiles {
		abs, err := o.expand(lf)
		if err != nil {
			return nil, err
		}
		locks = append(locks, abs)
	}
	sort.Strings(locks)
	for _, abs := range locks {
		// 私有锁在 <inst>/locks：它不是应用数据，不该混进 root/（§4 布局）。
		src := filepath.Join(in.Dir(), "locks", lockFileName(abs))
		args = append(args, "--bind", src, abs)
	}

	// ⑨⑩ 私有 runtime / 私有 DBus 总线。isolate_dbus 隐含 isolate_runtime：
	// 不隔离 runtime 时 <runtime> 是宿主 $XDG_RUNTIME_DIR，bwrap 会在宿主建 bus
	// 占位文件，正好压住真实的 session bus socket（§3.1 ⑩）。
	if needRuntime {
		args = append(args,
			// 源是实例目录（持久层），目标是命名空间里的宿主 runtime 路径。
			// 写成 Join(in.Root, "runtime") 会让源变成 <inst>/root/runtime——那个目录
			// 根本不存在，bwrap 遇到不存在的源会整条失败（§3.2 第 3 条）。
			"--bind", filepath.Join(in.Dir(), "runtime"), o.RuntimeDir,
			"--setenv", "XDG_RUNTIME_DIR", o.RuntimeDir, // 漏了这行，应用仍按旧值找 socket
		)
		for _, s := range o.RuntimeSocks {
			// 私有总线**确实在跑**时不能把宿主 bus 带进来：那等于把单例检测
			// 又接回宿主（§6.2）。总线启动失败（DBusAddress 为空）时是明确的
			// 降级路径：必须把宿主 bus 绑回来，否则 runtime 目录被换成空目录后
			// 应用继承的宿主地址指向一个不存在的 socket，工具却打印"共用总线"。
			// 判据是"直接位于宿主 runtime 下的 bus"；旧写法取了两级 Dir，
			// 对 /run/user/1000/bus 永远不成立（二级 Dir 是 /run/user）。
			if p.IsolateDBus && o.DBusAddress != "" && filepath.Base(s.Src) == "bus" && filepath.Dir(s.Src) == o.RuntimeDir {
				continue
			}
			// 只读、只 bind socket/目录本身：把整个 $XDG_RUNTIME_DIR 带进来
			// 等于把宿主单例 socket 一并带回来，等于没隔离（§6.2）。
			args = append(args, "--ro-bind-try", s.Src, s.Dst)
		}
	}
	if p.IsolateDBus && o.DBusAddress != "" {
		appAddr := o.DBusAppAddress
		if appAddr == "" {
			appAddr = o.DBusAddress
		}
		sock := o.BusSock
		if sock == "" {
			sock = filepath.Join(in.Dir(), "dbus", "bus")
		}
		args = append(args,
			"--bind", sock, filepath.Join(o.RuntimeDir, "bus"),
			"--setenv", "DBUS_SESSION_BUS_ADDRESS", appAddr,
		)
	}

	// ⑪ AC_HOME 生效时必须把 HOME 也换掉，否则应用仍按真实 $HOME 找路径，
	// 隔离测试会"看起来成功"（§6.4 最危险的假通过）。
	if o.Home != "" && o.RealHome != "" && filepath.Clean(o.Home) != filepath.Clean(o.RealHome) {
		args = append(args, "--setenv", "HOME", o.Home)
	}

	// ⑫ 档案 env：按 key 字典序输出。Go map 迭代随机，不排序则同一条档案
	// 每次生成的命令都不同，doctor 输出无法 diff、golden 断言无法写。
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--setenv", k, p.Env[k])
	}
	cwd := p.Cwd
	if strings.TrimSpace(cwd) == "" {
		cwd = o.Home
	} else {
		abs, err := o.expand(cwd)
		if err != nil {
			return nil, err
		}
		cwd = abs
	}
	// cwd 落在 AC_BASE 里时，沙箱内的 Base 已被 tmpfs 遮蔽，chdir 必失败。
	// 这里只做词法比较（本函数不探文件系统）；core.Run 会在调用前用 Real
	// 再兜一次，doctor 与 run 因此打印/执行同一条命令。
	if base := o.baseReal(); base != "" {
		if c := filepath.Clean(cwd); c == base || IsUnder(base, c) {
			cwd = o.Home
		}
	}
	args = append(args, "--chdir", cwd)

	// ⑬ 兜底：只保证直接子进程。真正的生命周期由 run 常驻父进程 + 进程组 kill 保证。
	args = append(args, "--die-with-parent")
	return args, nil
}

// BwrapCommand 返回可直接 exec 的完整命令：bwrap <args> -- <exec> <exec_args...>。
// run 与 doctor 共用它，doctor 才能打印出"真的会执行的那条命令"。
func BwrapCommand(p Profile, in Instance, o BuildOptions) ([]string, error) {
	args, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		return nil, err
	}
	cmd := append([]string{"bwrap"}, args...)
	cmd = append(cmd, "--", p.Exec)
	cmd = append(cmd, p.ExecArgs...)
	return cmd, nil
}

// instanceChild 把宿主绝对路径映射到实例目录下：/home/u/.config/App →
// <inst>/root/home/u/.config/App。
//
// 必须用 TrimPrefix 而不是 filepath.Join(in.Root, abs)：Join 遇到绝对路径会
// **丢弃前半段**，源直接指向宿主真实路径——隔离静默失效，且没有任何报错。
func instanceChild(root, abs string) (string, error) {
	rel := strings.TrimPrefix(filepath.Clean(abs), string(filepath.Separator))
	src := filepath.Join(root, rel)
	if !IsUnder(root, src) {
		return "", errInternal("err.internal.bind_escape", map[string]any{"root": root, "path": abs}, nil)
	}
	return src, nil
}

// lockFileName 是私有锁文件名：hex(sha256(cleanAbsTarget))[:16] + ".lock"。
func lockFileName(target string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(target)))
	return hex.EncodeToString(sum[:])[:16] + ".lock"
}

// RuntimeSocketCandidates 返回应该只读 bind 进私有 runtime 的宿主 socket 清单
// （§6.2 的表格）。存在性判断留给调用方：这里只给候选，不 stat。
func RuntimeSocketCandidates(runtimeDir string) []BindPair {
	cands := []BindPair{
		{Src: filepath.Join(runtimeDir, "pulse", "native"), Dst: filepath.Join(runtimeDir, "pulse", "native")},
		{Src: filepath.Join(runtimeDir, "pipewire-0"), Dst: filepath.Join(runtimeDir, "pipewire-0")},
		{Src: filepath.Join(runtimeDir, "bus"), Dst: filepath.Join(runtimeDir, "bus")},
		{Src: filepath.Join(runtimeDir, "dconf", "user"), Dst: filepath.Join(runtimeDir, "dconf", "user")},

		{Src: filepath.Join(runtimeDir, "X11-unix"), Dst: filepath.Join(runtimeDir, "X11-unix")},
	}
	// at-spi 的实际文件名是 bus 或 bus_0（通配匹配，只认 bus 会让读屏失效）；
	// wayland-<n> 同理按通配匹配编号。
	if ents, err := os.ReadDir(filepath.Join(runtimeDir, "at-spi")); err == nil {
		for _, ent := range ents {
			if strings.HasPrefix(ent.Name(), "bus") {
				p := filepath.Join(runtimeDir, "at-spi", ent.Name())
				cands = append(cands, BindPair{Src: p, Dst: p})
			}
		}
	}
	if ents, err := os.ReadDir(runtimeDir); err == nil {
		for _, ent := range ents {
			name := ent.Name()
			if strings.HasPrefix(name, "wayland-") && !strings.HasSuffix(name, ".lock") {
				cands = append(cands, BindPair{
					Src: filepath.Join(runtimeDir, name),
					Dst: filepath.Join(runtimeDir, name),
				})
			}
		}
	}
	return cands
}

// FilterExisting 只保留源确实存在的 bind 对。宿主 socket 缺失是常态
// （没跑 PulseAudio 就没有 pulse/native），不能因此让整个 run 失败。
func FilterExisting(pairs []BindPair) []BindPair {
	out := make([]BindPair, 0, len(pairs))
	for _, p := range pairs {
		if _, err := os.Lstat(p.Src); err == nil {
			out = append(out, p)
		}
	}
	return out
}
