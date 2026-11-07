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
// 纯函数：不读环境变量、不探文件系统。存在性与权限由 PrepareBindTargets
// 前置完成——这里 stat 一次就会让"能不能构造参数"依赖于运行时状态，
// doctor 也就无法离线展示将要执行的命令。
func BuildBwrapArgs(p Profile, in Instance, o BuildOptions) ([]string, error) {
	if err := ValidateProfile(p, o); err != nil {
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

	// ④.5 审计层（§6.5）：只在 `run --audit` 时存在。顺序写死——
	//   overlay 先罩住 HOME，再把**公共目录 / AC_BASE** 绑回真实位置，
	//   最后才是 ⑤ 的实例私有目录。反过来（实例 bind 在前）会被公共目录的
	//   bind 盖掉，应用数据反而落进 upper，verify 会把正常数据当越界。
	if o.Audit != nil {
		args = append(args, "--bind", o.Audit.Merged, o.Home)
		for _, pb := range o.Audit.PublicBinds {
			args = append(args, "--bind", pb.Src, pb.Dst)
		}
		if o.Base != "" {
			args = append(args, "--bind", o.Base, o.Base)
		}
	}

	// ⑤ 仅 paths 声明的目录，父先子后（normalizePaths 已给出全序）。
	for _, abs := range paths {
		src, err := instanceChild(in.Root, abs)
		if err != nil {
			return nil, err
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
	needRuntime := p.IsolateRuntime || (p.IsolateDBus && o.DBusAddress != "")
	if needRuntime {
		args = append(args,
			// 源是实例目录（持久层），目标是命名空间里的宿主 runtime 路径。
			// 写成 Join(in.Root, "runtime") 会让源变成 <inst>/root/runtime——那个目录
			// 根本不存在，bwrap 遇到不存在的源会整条失败（§3.2 第 3 条）。
			"--bind", filepath.Join(in.Dir(), "runtime"), o.RuntimeDir,
			"--setenv", "XDG_RUNTIME_DIR", o.RuntimeDir, // 漏了这行，应用仍按旧值找 socket
		)
		for _, s := range o.RuntimeSocks {
			// 私有总线时不能把宿主 bus 带进来：那等于把单例检测又接回宿主（§6.2）。
			if p.IsolateDBus && filepath.Base(s.Src) == "bus" && filepath.Dir(filepath.Dir(s.Src)) == o.RuntimeDir {
				continue
			}
			// 只读、只 bind socket/目录本身：把整个 $XDG_RUNTIME_DIR 带进来
			// 等于把宿主单例 socket 一并带回来，等于没隔离（§6.2）。
			args = append(args, "--ro-bind-try", s.Src, s.Dst)
		}
	}
	if p.IsolateDBus && o.DBusAddress != "" {
		args = append(args,
			"--bind", filepath.Join(in.Dir(), "dbus", "bus"), filepath.Join(o.RuntimeDir, "bus"),
			"--setenv", "DBUS_SESSION_BUS_ADDRESS", o.DBusAddress,
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
