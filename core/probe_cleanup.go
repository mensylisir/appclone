package core

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"sort"
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
	root, err := os.MkdirTemp("/var/tmp", fmt.Sprintf("appclone-probe-%d-", e.UID))
	if err != nil {
		return "", errEnv("err.env.mkdir", map[string]any{"path": "/var/tmp"}, err)
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
	dirs, err := filepath.Glob(fmt.Sprintf("/var/tmp/appclone-probe-%d-*", e.UID))
	if err != nil {
		return nil
	}
	for _, d := range dirs {
		merged := filepath.Join(d, "merged")
		if mounted(merged) {
			if err := umount(merged); err != nil {
				return errEnv("err.probe.stale_mount", map[string]any{"path": merged}, err)
			}
		}
		upper := filepath.Join(d, "upper")
		entries, present, rerr := readUpperTree(upper)
		if rerr != nil || !present || len(entries) == 0 {
			os.RemoveAll(d)
			continue
		}
		// 有内容：可能含用户文件，改名保留而不是删。
		q := fmt.Sprintf("/var/tmp/appclone-quarantine-%d-%s", e.UID,
			time.Now().UTC().Format("20060102T150405")+"-"+randSuffix())
		if err := os.Rename(d, q); err != nil {
			return errEnv("err.probe.quarantine_failed", map[string]any{"path": d}, err)
		}
	}
	return nil
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
