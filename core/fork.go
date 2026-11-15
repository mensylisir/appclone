package core

// Fork instances use durable private snapshots as lower layers; root remains
// the existing writable upper and .fork contains only temporary mount state.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func forkDir(in Instance) string { return filepath.Join(in.Dir(), ".fork") }

// Any required snapshot/mount failure aborts before the application starts.
func (e *Env) forkMounts(p Profile, in Instance, stop func() bool, warn func(string)) (map[string]string, func() error, func() error, error) {
	noop := func() error { return nil }
	if !in.Fork {
		return nil, noop, noop, nil
	}
	var err error
	e, in, err = e.pinForkInstance(in)
	if err != nil {
		return nil, noop, noop, forkPreparationError(err)
	}
	paths, err := normalizePaths(p.Paths, e.BuildOptions())
	if err != nil {
		return nil, noop, noop, err
	}
	if err := checkForkHashes(paths); err != nil {
		return nil, noop, noop, forkPreparationError(err)
	}
	aliases, err := e.forkAliases(p, paths)
	if err != nil {
		return nil, noop, noop, err
	}
	creds, err := e.forkCredentials(p)
	if err != nil {
		return nil, noop, noop, err
	}
	mounter := e.Mounter
	if mounter == nil {
		mounter = fuseOverlayMounter{}
	}
	if err := mounter.Available(); err != nil {
		return nil, noop, noop, err
	}
	if err := e.forkRun.check(); err != nil {
		return nil, noop, noop, forkPreparationError(err)
	}
	instPath := e.forkRun.instance.canonical
	inst, err := openForkRoot(instPath)
	if err != nil {
		return nil, noop, noop, forkPreparationError(err)
	}
	base := filepath.Join(instPath, ".fork")
	guard := newForkRuntimeGuard(inst, instPath)
	guard.identity = e.forkRun
	type ownedMount struct {
		path string
		stop func() error
	}
	var unmounts []ownedMount
	closed := false
	var cleanupErr error
	cleanup := func() error {
		if closed {
			return cleanupErr
		}
		closed = true
		defer inst.Close()
		e.forkCheckpoint("before-fork-cleanup")
		if err := guard.check(); err != nil {
			cleanupErr = err
			return err
		}
		for i := len(unmounts) - 1; i >= 0; i-- {
			if err := guard.check(); err != nil {
				cleanupErr = err
				return err
			}
			if err := unmounts[i].stop(); err != nil {
				cleanupErr = err
			} else {
				delete(guard.mounts, unmounts[i].path)
			}
		}
		mounted, err := checkedMountUnder(base)
		if err != nil {
			cleanupErr = err
		}
		if mounted {
			cleanupErr = errors.New("fork mount remains active")
		}
		if cleanupErr == nil {
			cleanupErr = guard.check()
			if cleanupErr == nil {
				cleanupErr = inst.RemoveAll(".fork")
			}
		}
		return cleanupErr
	}
	fail := func(err error) (map[string]string, func() error, func() error, error) {
		if cerr := cleanup(); cerr != nil {
			return nil, noop, noop, forkCleanupError(cerr)
		}
		return nil, noop, noop, forkPreparationError(err)
	}
	e.forkCheckpoint("before-fork-reset")
	if err := e.forkRun.check(); err != nil {
		inst.Close()
		return nil, noop, noop, forkPreparationError(err)
	}
	if err := resetForkRoot(inst, instPath, ".fork", e.forkRun); err != nil {
		inst.Close()
		return nil, noop, noop, forkPreparationError(err)
	}
	lowers, err := e.prepareForkSnapshot(p, in, paths, creds, stop)
	if err != nil {
		return fail(err)
	}
	if err := forkMkdirAll(inst, ".fork"); err != nil {
		return fail(err)
	}
	if err := guard.remember(".fork"); err != nil {
		return fail(err)
	}
	sources := map[string]string{}
	for _, abs := range paths {
		if err := forkStopped(stop); err != nil {
			return fail(err)
		}
		upper, err := instanceChild(filepath.Join(instPath, "root"), abs)
		if err != nil {
			return fail(err)
		}
		sources[abs] = upper // Explicit empty/file/credential decisions also have a bind.
		upperRel, err := filepath.Rel(instPath, upper)
		if err != nil {
			return fail(err)
		}
		if err := prepareForkUpper(inst, upperRel, abs); err != nil {
			return fail(err)
		}
		if err := guard.remember(upperRel); err != nil {
			return fail(err)
		}
		lower := lowers[abs]
		if lower == "" {
			continue
		}
		workRel := filepath.Join(".fork", forkHash(abs), "work")
		mergedRel := filepath.Join(".fork", forkHash(abs), "merged")
		ids := map[string]os.FileInfo{}
		for _, rel := range []string{upperRel, workRel, mergedRel} {
			if err := forkMkdirAll(inst, rel); err != nil {
				return fail(err)
			}
			fi, err := inst.Lstat(rel)
			if err != nil {
				return fail(err)
			}
			ids[rel] = fi
			if err := guard.remember(rel); err != nil {
				return fail(err)
			}
		}
		lowerRel, err := filepath.Rel(instPath, lower)
		if err != nil {
			return fail(err)
		}
		fi, err := inst.Lstat(lowerRel)
		if err != nil {
			return fail(err)
		}
		ids[lowerRel] = fi
		if err := guard.remember(lowerRel); err != nil {
			return fail(err)
		}
		rootID, err := inst.Stat(".")
		if err != nil {
			return fail(err)
		}
		e.forkCheckpoint("before-mount")
		if err := e.checkForkPaths(p, paths, aliases); err != nil {
			return fail(err)
		}
		current, err := os.Lstat(instPath)
		if err != nil {
			return fail(err)
		}
		if !os.SameFile(rootID, current) {
			return fail(errors.New("instance root changed before mount"))
		}
		for rel, want := range ids {
			if err := forkNoSymlinks(inst, rel); err != nil {
				return fail(err)
			}
			got, err := inst.Lstat(rel)
			if err != nil {
				return fail(err)
			}
			if !os.SameFile(want, got) {
				return fail(errors.New("mount path changed"))
			}
		}
		work, merged := filepath.Join(instPath, workRel), filepath.Join(instPath, mergedRel)
		if err := guard.beginMount(); err != nil {
			return fail(err)
		}
		unmount, err := mounter.Mount(lower, upper, work, merged)
		if err != nil {
			if mounted, merr := checkedMountUnder(merged); merr != nil {
				guard.retain = errors.Join(err, merr)
			} else if mounted {
				guard.retain = errEnv("err.fork.mount_pending", nil, err)
			}
			return fail(err)
		}
		if gerr := guard.mounted(mergedRel); gerr != nil {
			guard.retain = gerr
			return fail(gerr)
		}
		if err := guard.confirmMount(); err != nil {
			guard.retain = err
			return fail(err)
		}
		unmounts = append(unmounts, ownedMount{path: merged, stop: unmount})
		sources[abs] = merged
	}
	e.forkCheckpoint("after-mount")
	if err := e.checkForkPaths(p, paths, aliases); err != nil {
		return fail(err)
	}
	if err := guard.check(); err != nil {
		return fail(err)
	}
	return sources, cleanup, guard.check, nil
}

// lexicalCredentialPaths 返回凭据路径的词法绝对形态（不解析软链）。
// fork 的阻断必须同时看词法与真实形态，见 forkMounts 的注释。
func (e *Env) lexicalCredentialPaths(raw []string) []string {
	var out []string
	for _, s := range raw {
		if abs, err := e.Resolver.expandLexical(s); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// credentialBlocksFork 判定一条 lower 是否因凭据范围而不能克隆（§10.2）：
// lower 等于凭据路径、包含凭据路径（嵌套）或位于凭据路径之下都不挂。
func credentialBlocksFork(lower string, creds []string) (bool, string) {
	for _, cp := range creds {
		switch {
		case samePathLoose(lower, cp):
			return true, "「" + lower + "」是登录状态目录，克隆时不继承（避免把登录态带进分身）"
		case isUnderLoose(lower, cp):
			return true, "「" + lower + "」包含登录状态目录，克隆时不继承（避免把登录态带进分身）"
		case isUnderLoose(cp, lower):
			return true, "「" + lower + "」位于登录状态目录内，克隆时不继承"
		}
	}
	return false, ""
}

// resetForkBase 清掉上一次运行留下的挂载点与工作区。
//
// 顺序不能反：先卸挂载，再删目录。带挂载的 RemoveAll 会进入 merged 视图，
// 把 lower（主实例的真实数据）当成待删文件——这正是 §3.2.3 踩过的坑。
func resetForkBase(base string) error {
	parent, err := checkedOrdinaryPath(filepath.Dir(base), true)
	if err != nil {
		return err
	}
	root, err := openForkRoot(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return resetForkRoot(root, parent, filepath.Base(base), nil)
}

func resetForkRoot(root *os.Root, parent, rel string, identity *forkRunIdentity) error {
	if err := identity.check(); err != nil {
		return err
	}
	base := filepath.Join(parent, rel)
	table, err := checkedMountTable()
	if err != nil {
		return err
	}
	if err := checkForkStarting(root, parent, table); err != nil {
		return err
	}
	if err := checkedRootPath(root, parent); err != nil {
		return err
	}
	if st, err := os.Lstat(base); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("fork mount root is a symbolic link")
	}
	if _, err := root.Lstat(filepath.Join(rel, "mount-pending")); err == nil {
		for point := range table {
			if IsUnder(base, point) {
				return errEnv("err.fork.mount_pending", nil, nil)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// 布局是 <base>/<hash>/merged（每个 paths 条目的挂载点各占一层），
	// 所以先扫 hash 目录，再判断里面的 merged。
	f, err := root.Open(rel)
	if err != nil {
		// 不存在 = 没跑过；不是目录 = 应用自己在实例目录里建了个叫 .fork
		// 的文件（整条 `--bind / /` 是 rw，它做得到）。两种都不承载挂载，
		// 不能因此挡住删除。
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		return err
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	guard := newForkRuntimeGuard(root, parent)
	guard.identity = identity
	if err := guard.remember(rel); err != nil {
		return err
	}
	for path, record := range table {
		if IsUnder(base, path) {
			guard.mounts[path] = record
		}
	}
	for _, ent := range ents {
		if err := guard.remember(filepath.Join(rel, ent.Name())); err != nil {
			return err
		}
	}
	// 一条卸不掉不能挡住其余路径：先把所有能卸的都卸掉，再报第一个错误。
	var firstErr error
	for _, ent := range ents {
		p := filepath.Join(base, ent.Name(), "merged")
		// stale 现场只看 mountinfo：fuse daemon 已死时任何 stat 都返回
		// ENOTCONN，会把仍挂着的条目误判成"没挂载"，接着 RemoveAll 就会
		// 穿过挂载点遍历 lower。
		present := false
		for mp := range table {
			for _, form := range pathForms(p) {
				if mp == form {
					present = true
				}
			}
		}
		if present {
			if err := guard.check(); err != nil {
				return err
			}
			if err := umount(p); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				delete(guard.mounts, p)
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}
	// 兜底：即使目录布局变了或出现未知条目，只要 mountinfo 里还有 base
	// 之下的挂载点就绝不 RemoveAll。
	remaining, err := checkedMountUnder(base)
	if err != nil {
		return err
	}
	if remaining {
		return errors.New("仍有挂载点在 " + base + " 下，拒绝删除")
	}
	if err := guard.check(); err != nil {
		return err
	}
	return root.RemoveAll(rel)
}

// procMountInfo 是 mountinfo 路径（测试可替换成 fixture）。
var procMountInfo = "/proc/self/mountinfo"

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
