package core

// 克隆起步（§8.1）：`add --fork` 建的实例在每次 run 时，把主实例的真实数据
// 目录当只读 lower、实例 root 当持久 upper，用 fuse-overlayfs 挂出一份写时
// 复制的视图。应用第一次启动看到的就和主实例当前状态一致，之后的修改只落
// 在分身自己的 upper 里——不需要 root，主实例数据也不会被碰。
//
// 为什么不复制文件：聊天/浏览器的数据动辄几 GB，复制既慢又占双份空间；
// overlay 的 copy-up 是按需的。代价是每次 run 多几次挂载，run 结束就卸掉。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// forkDir 是实例内放临时挂载点的目录。它随时可删（不在 root/ 里），
// 名字带点是为了让人一眼看出它不是用户数据。
func forkDir(in Instance) string { return filepath.Join(in.Dir(), ".fork") }

// forkMounts 为 fork 实例建立所有 overlay，返回 bind 源映射（宿主路径 →
// 挂载点）与清理函数。任一路径挂不上只降级该路径并告警，不阻断启动：
// “少继承一些数据”远好于“应用起不来”。
// stop 在每条路径挂载前检查（ctx 取消或收到停止信号）。Mount 本身是同步
// 5 秒上限，无法中途打断；这里保证不会把 N×5 秒全部走完才响应停止。
func (e *Env) forkMounts(p Profile, in Instance, stop func() bool, warn func(string)) (map[string]string, func()) {
	if !in.Fork {
		return nil, func() {}
	}
	say := func(msg string) {
		if warn != nil {
			warn(msg)
		}
	}
	paths, err := normalizePaths(p.Paths, e.BuildOptions())
	if err != nil || len(paths) == 0 {
		return nil, func() {}
	}
	mounter := e.Mounter
	if mounter == nil {
		mounter = fuseOverlayMounter{}
	}
	if err := mounter.Available(); err != nil {
		say("fuse-overlayfs 不可用，这个分身无法继承主实例数据，将从空数据启动")
		return nil, func() {}
	}
	base := forkDir(in)
	// 上次 run 被 SIGKILL 时可能留下挂载点。必须先卸再删：带着挂载
	// RemoveAll 会顺着 merged 遍历 lower，删的是主实例的用户数据。
	if err := resetForkBase(base); err != nil {
		say("无法清理上次运行留下的挂载点，这个分身将从空数据启动")
		return nil, func() {}
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		say("无法创建克隆挂载目录，这个分身将从空数据启动")
		return nil, func() {}
	}

	sources := map[string]string{}
	var unmounts []func() error
	cleanup := func() {
		ok := true
		for i := len(unmounts) - 1; i >= 0; i-- {
			if err := unmounts[i](); err != nil {
				ok = false
			}
		}
		// 有卸载失败、或 mountinfo 里仍有 base 之下的挂载点时不碰 base：
		// merged 可能仍挂着，RemoveAll 会穿过它（与 resetForkBase 同一硬不变量）。
		if ok && !mountUnder(base) {
			_ = os.RemoveAll(base)
		}
	}

	for _, abs := range paths {
		if stop != nil && stop() {
			break
		}
		// 只对“主实例里已经有内容”的目录做 overlay：空目录挂一份空视图
		// 除了多一次挂载没有任何收益。
		if st, serr := os.Stat(abs); serr != nil || !st.IsDir() || !dirHasEntries(abs) {
			continue
		}
		upper, err := instanceChild(in.Root, abs)
		if err != nil {
			continue
		}
		// upper 落在 lower 之内会让 overlay 递归（挂载不报错，视图 ELOOP）。
		// 典型触发：AC_BASE 位于档案声明的某个目录之下。两种路径形态都要比：
		// AC_BASE 经软链、档案写的是解析后祖先时，纯词法比较会漏。
		if lowerContainsUpper(abs, upper) {
			say("「" + abs + "」包含分身自己的数据目录，无法克隆起步，该路径将不继承主实例数据")
			continue
		}
		if err := os.MkdirAll(upper, 0o700); err != nil {
			say("无法为「" + abs + "」准备分身数据目录，该路径将不继承主实例数据")
			continue
		}
		wd := filepath.Join(base, forkHash(abs))
		work := filepath.Join(wd, "work")
		merged := filepath.Join(wd, "merged")
		if err := os.MkdirAll(work, 0o700); err != nil {
			say("无法为「" + abs + "」准备挂载工作区，该路径将不继承主实例数据")
			continue
		}
		if err := os.MkdirAll(merged, 0o700); err != nil {
			say("无法为「" + abs + "」准备挂载点，该路径将不继承主实例数据")
			continue
		}
		unmount, merr := mounter.Mount(abs, upper, work, merged)
		if merr != nil {
			say("无法为「" + abs + "」挂载主实例数据，该路径将不继承")
			// 极窄竞态：Mount 已报错（典型是超时），但挂载点刚好生效了。
			// 这时删 wd 会穿过 merged 遍历 lower（§3.2.3 同一事故）。挂着
			// 就交给 cleanup 先卸；卸不掉则保留 .fork/ 等下一轮 reset。
			if mountPointIn(merged) {
				unmounts = append(unmounts, func() error { return umount(merged) })
				continue
			}
			_ = os.RemoveAll(wd)
			continue
		}
		sources[abs] = merged
		unmounts = append(unmounts, unmount)
	}
	if len(sources) == 0 {
		cleanup()
		return nil, func() {}
	}
	return sources, cleanup
}

// resetForkBase 清掉上一次运行留下的挂载点与工作区。
//
// 顺序不能反：先卸挂载，再删目录。带挂载的 RemoveAll 会进入 merged 视图，
// 把 lower（主实例的真实数据）当成待删文件——这正是 §3.2.3 踩过的坑。
func resetForkBase(base string) error {
	// 布局是 <base>/<hash>/merged（每个 paths 条目的挂载点各占一层），
	// 所以先扫 hash 目录，再判断里面的 merged。
	ents, err := os.ReadDir(base)
	if err != nil {
		// 不存在 = 没跑过；不是目录 = 应用自己在实例目录里建了个叫 .fork
		// 的文件（整条 `--bind / /` 是 rw，它做得到）。两种都不承载挂载，
		// 不能因此挡住删除。
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		return err
	}
	// 一条卸不掉不能挡住其余路径：先把所有能卸的都卸掉，再报第一个错误。
	var firstErr error
	for _, ent := range ents {
		p := filepath.Join(base, ent.Name(), "merged")
		// stale 现场只看 mountinfo：fuse daemon 已死时任何 stat 都返回
		// ENOTCONN，会把仍挂着的条目误判成"没挂载"，接着 RemoveAll 就会
		// 穿过挂载点遍历 lower。
		if mountPointIn(p) {
			if err := umount(p); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}
	// 兜底：即使目录布局变了或出现未知条目，只要 mountinfo 里还有 base
	// 之下的挂载点就绝不 RemoveAll。
	if mountUnder(base) {
		return errors.New("仍有挂载点在 " + base + " 下，拒绝删除")
	}
	return os.RemoveAll(base)
}

// procMountInfo 是 mountinfo 路径（测试可替换成 fixture）。
var procMountInfo = "/proc/self/mountinfo"

// mountPointIn 只查 mountinfo，不碰挂载点本身：判断 stale 挂载时任何
// 对 merged 的 stat/open 都会得到 ENOTCONN。
func mountPointIn(path string) bool {
	data, err := os.ReadFile(procMountInfo)
	if err != nil {
		return false
	}
	return mountInfoHas(string(data), path)
}

// mountUnder 报告 mountinfo 里是否有挂载点等于 base 或位于 base 之下。
func mountUnder(base string) bool {
	data, err := os.ReadFile(procMountInfo)
	if err != nil {
		return false
	}
	forms := pathForms(base)
	for _, line := range strings.Split(string(data), "\n") {
		mp, ok := mountPointOf(line)
		if !ok {
			continue
		}
		for _, form := range forms {
			if IsUnder(form, mp) {
				return true
			}
		}
	}
	return false
}

// mountInfoHas 逐行比对挂载点字段。不能直接 strings.Contains 拼路径：
// 内核会把空格/制表符/换行/反斜杠转义成 \040 \011 \012 \134，AC_BASE 或
// HOME 带空格时（例如 "My Data"）原样路径永远匹配不上，stale 挂载会被
// 漏判，随后 RemoveAll 穿过挂载点删掉主实例数据。
func mountInfoHas(data, path string) bool {
	forms := pathForms(path)
	for _, line := range strings.Split(data, "\n") {
		mp, ok := mountPointOf(line)
		if !ok {
			continue
		}
		for _, form := range forms {
			if mp == form {
				return true
			}
		}
	}
	return false
}

// mountPointOf 取出 mountinfo 行的第 5 个字段并解码转义。
//
// 必须按 ASCII 单空格切分：strings.Fields 会按 Unicode 空白分词，而内核
// 只转义 ASCII 空格/制表/换行/反斜杠——路径里的 U+3000/NBSP 会被当成
// 分隔符，字段错位后整条挂载点认不出来。
func mountPointOf(line string) (string, bool) {
	f := strings.SplitN(line, " ", 6)
	if len(f) < 5 {
		return "", false
	}
	return unescapeMountPath(f[4]), true
}

// pathForms 返回词法路径与（尽力求出的）软链解析后路径。
//
// mountinfo 记录的是 d_path（软链已解析），而调用方手里是词法路径。AC_BASE
// 或 HOME 经过软链（/home → /var/home）时只比一种形态会永远判不中：
// 挂载轮询超时、stale 漏判、RemoveAll 穿过挂载点。两种都接受。
func pathForms(path string) []string {
	clean := filepath.Clean(path)
	forms := []string{clean}
	if real, ok := realPathBestEffort(clean); ok && real != clean {
		forms = append(forms, real)
	}
	return forms
}

// realPathBestEffort 解析软链，但**从不解析最后一段本身**：path 通常是
// merged 挂载点，fuse daemon 卡死时 EvalSymlinks 会卡在一个不可中断的
// stat 上。mountinfo 的 d_path 对挂载点自身也不做软链解析，所以只需要把
// 父目录解析成真实路径、再把最后一段拼回去。父目录也缺失时逐级上退。
func realPathBestEffort(path string) (string, bool) {
	clean := filepath.Clean(path)
	dir, base := filepath.Split(clean)
	for {
		d := filepath.Clean(dir)
		if real, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(real, base), true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		base = filepath.Join(filepath.Base(d), base)
		dir = parent
	}
}

// lowerContainsUpper 判断上层目录是否落在下层之内（overlay 递归）。
// 词法与真实形态各比一次：AC_BASE 经软链、档案写解析后祖先时，纯词法会漏。
func lowerContainsUpper(lower, upper string) bool {
	for _, l := range pathForms(lower) {
		for _, u := range pathForms(upper) {
			if IsUnder(l, u) {
				return true
			}
		}
	}
	return false
}

// mountEscapes 按 proc(5) 规定的四种转义解码挂载点。单遍替换即可：
// 反斜杠本身也是 \134，不会被二次解释成别的转义。
var mountEscapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func unescapeMountPath(s string) string { return mountEscapes.Replace(s) }

// dirHasEntries 判断目录里是否至少有一项。用 Readdirnames(1) 而不是读全表：
// 应用数据目录可能有上万个文件，这里只回答“空不空”。
func dirHasEntries(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}

// forkHash 给每个宿主路径一个短且稳定的目录名。用路径哈希而不是下标或
// 转义路径：路径里有空格/中文时 directory 名也不会失控，且顺序变化不影响
// 旧挂载点的识别。
func forkHash(abs string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return hex.EncodeToString(sum[:])[:8]
}
