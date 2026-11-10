package core

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// ProbeContaminated 是 §3.2.1 的反向断言：**被 bind 回真实的那些路径**出现在
// upper 里，说明 bind 没生效（或应用绕过了 bind）。纯函数，与删不删现场分开。
//
// 三个来源都要查：公共目录、AC_BASE、shared_writes。只查公共目录时，
// "AC_BASE 没 bind 上"会让工具自己的写入进 upper，最后随 quarantine 一起消失。
func (e *Env) ProbeContaminated(entries []UpperEntry, publicBinds []BindPair) []string {
	var mustBind []string
	for _, pb := range publicBinds {
		mustBind = append(mustBind, pb.Dst)
	}
	if e.Base != "" {
		mustBind = append(mustBind, e.Base)
	}
	mustBind = append(mustBind, e.probeSharedWrites()...)

	var out []string
	seen := map[string]bool{}
	for _, en := range entries {
		abs := filepath.Clean(filepath.Join(e.Resolver.Home, filepath.FromSlash(en.Path)))
		for _, target := range mustBind {
			// loose：$HOME 是软链时（/home -> /var/home）词法比较会整体漏判，
			// 漏判的后果是删掉可能含用户文件的 upper。
			if !isUnderLoose(target, abs) {
				continue
			}
			if !seen[abs] {
				seen[abs] = true
				out = append(out, abs)
			}
			break
		}
	}
	sort.Strings(out)
	return out
}

// newProbeWorkDir 建探测写层目录，并断言 upper 不嵌在 lower 里。
//
// overlay 的 upper 落在它自己的 lower 下会让视图递归：`ls merged/<更深层>` 直接
// ELOOP，而且删不掉（§3.2 的实测记录）。work/upper 在同一个 MkdirTemp 下，
// 同文件系统这一条是隐含成立的。
func (e *Env) newProbeWorkDir(app string) (string, error) {
	root, err := os.MkdirTemp(e.tmpRoot(), fmt.Sprintf("appclone-probe-%d-", e.UID))
	if err != nil {
		return "", errEnv("err.env.mkdir", map[string]any{"path": e.tmpRoot()}, err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		os.RemoveAll(root)
		return "", errEnv("err.env.chmod", map[string]any{"path": root}, err)
	}
	for _, sub := range []string{"upper", "work", "merged", "runtime", "dbus"} {
		if err := os.Mkdir(filepath.Join(root, sub), 0o700); err != nil {
			os.RemoveAll(root)
			return "", errEnv("err.env.mkdir", map[string]any{"path": filepath.Join(root, sub)}, err)
		}
	}
	up := Real(filepath.Join(root, "upper"))
	lower := Real(e.Resolver.Home)
	if IsUnder(lower, up) || up == lower {
		os.RemoveAll(root)
		return "", errEnv("err.probe.workdir_in_home", map[string]any{"path": up, "home": lower}, nil)
	}
	return root, nil
}

// sweepStaleProbes 清掉上一轮残留（§3.2.3）：仍挂载的先 `fusermount -uz`，
// 然后**先做污染检查再删**——残留的 upper 里可能就有用户文件。
func (e *Env) sweepStaleProbes() error {
	if err := e.sweepProbeRoot(e.tmpRoot()); err != nil {
		return err
	}
	// ADR-033 之前写层根是 /var/tmp：升级上来的机器里可能留着崩溃残留
	// （含未卸载的挂载点）。它们再也不会被探测命中，必须一次性扫掉。
	// 只扫 appclone-probe-*，绝不碰 appclone-quarantine-*。
	// 测试注入 TmpRoot 时只扫注入根：生产旧根里是**真实**残留，不能让
	// 单测顺手删掉用户机器上的现场。
	if e.TmpRoot == "" && legacyTmpRoot != e.tmpRoot() {
		return e.sweepProbeRoot(legacyTmpRoot)
	}
	return nil
}

func (e *Env) sweepProbeRoot(root string) error {
	dirs, err := filepath.Glob(filepath.Join(root, fmt.Sprintf("appclone-probe-%d-*", e.UID)))
	if err != nil {
		return nil
	}
	for _, d := range dirs {
		// /tmp 与 /var/tmp 都是世界可写：只处理属主是本 uid 的**目录**。
		// 别的用户预置同名条目时，搬走/删除既可能被粘滞位拒绝，也会让我们
		// 把一个不属于自己的目录拷进 quarantine。
		if !dirOwnedBy(d, e.UID) {
			continue
		}
		merged := filepath.Join(d, "merged")
		if mounted(merged) {
			if err := umount(merged); err != nil {
				return errEnv("err.probe.stale_mount", map[string]any{"path": merged}, err)
			}
		}
		upper := filepath.Join(d, "upper")
		entries, present, rerr := readUpperTree(upper)
		// 读失败不能当"空"：看不见内容恰恰说明里面可能有用户文件
		// （与 sweepOneAudit 同一口径）。只有确认存在且为空才删。
		if rerr == nil && present && len(entries) == 0 {
			if err := os.RemoveAll(d); err != nil {
				return errEnv("err.env.write", map[string]any{"path": d}, err)
			}
			continue
		}
		if _, err := e.quarantineDir(d); err != nil {
			return errEnv("err.probe.quarantine_failed", map[string]any{"path": d}, err)
		}
	}
	return nil
}

// quarantineDir 把可能含用户文件的目录搬去保留区，并返回最终落点。
//
// 优先持久的 /var/tmp；写层根在 tmpfs 时（/tmp、$XDG_RUNTIME_DIR）跨设备
// 只能**拷贝**，而 chmod 000 的子树连属主都读不了——拷贝失败不能让现场卡死
// 后续探测/审计。此时退回写层根内的同文件系统 rename：名字仍唯一、内容不动，
// 只是持久性降到重启为止。
//
// 注意"拷贝成功、删源失败"（子目录 0555 且含文件）：那**不是**搬迁失败，
// 完整副本就在 q，必须把 q 交给用户；再拿残缺的源去 rename 只会指向副本。
func (e *Env) quarantineDir(src string) (string, error) {
	name := fmt.Sprintf("appclone-quarantine-%d-%s", e.UID,
		time.Now().UTC().Format("20060102T150405")+"-"+randSuffix())
	q := filepath.Join(e.quarantineRoot(), name)
	err := moveDir(src, q)
	if err == nil || dstComplete(err) {
		return q, nil
	}
	fallback := filepath.Join(e.tmpRoot(), name)
	if ferr := moveDir(src, fallback); ferr != nil && !dstComplete(ferr) {
		// 两个都失败：源必须原样留着（moveDir 保证拷贝未完成时不动源）。
		return "", err
	}
	return fallback, nil
}

// legacyTmpRoot 是 ADR-033 之前的写层根（/var/tmp）。变量化只为测试注入：
// 生产路径永远是 /var/tmp，单测不能去碰真实旧根。
var legacyTmpRoot = "/var/tmp"

// dirOwnedBy 判断路径是不是本 uid 的目录（不跟随软链）。世界可写目录里的
// 同名条目可能是别的用户放的，不能当成自己的残留处理。
func dirOwnedBy(path string, uid int) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}

// randSuffix 给 quarantine 名加随机尾巴：同一秒内两次命中污染时不能撞名
// （撞名的 Rename 在 Linux 上会直接覆盖空目录 / 失败）。
func randSuffix() string {
	var b [4]byte
	if _, err := unix.Getrandom(b[:], 0); err != nil {
		return fmt.Sprint(time.Now().UnixNano() % 100000)
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}
