package core

import (
	"debug/elf"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// keyring 证据的三种取值（方案 §6）。"" 表示没有证据。
const (
	KeyringEvidenceNone     = ""
	KeyringEvidenceLinked   = "linked"
	KeyringEvidenceObserved = "observed"
)

// KeyringCaps 是"本机能不能提供独立密钥环"的结论。CLI/GUI 共用这一份，
// 两边各算一次迟早会在"GUI 说可用、run 报缺 daemon"上分叉。
type KeyringCaps struct {
	Available bool
	Daemon    string // gnome-keyring-daemon 绝对路径
	DbusSend  string // dbus-send 绝对路径
	// DbusDaemon/Bwrap 不参与 Available（方案 §6 的默认策略只要求前两项），
	// 但 run 需要它们，预先解析好避免两处各写一套 PATH 候选。
	DbusDaemon string
	Bwrap      string
	// Missing 列出缺失的可执行文件名（给界面写"安装 xxx"用）。
	Missing []string
}

// keyringPathDirs 是能力解析的候选目录。**不读用户 PATH**：能力结论必须
// 与 run 的固定 PATH 一致，否则用户 PATH 里一个同名脚本就能让 GUI 显示
// "可用"、run 却起不来。`/usr/libexec` 是部分发行版放 daemon 的位置。
var keyringPathDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/libexec"}

// KeyringCapability 解析独立 keyring 需要的可执行文件。
func KeyringCapability() KeyringCaps {
	c := KeyringCaps{
		Daemon:     lookPathIn(keyringPathDirs, "gnome-keyring-daemon"),
		DbusSend:   lookPathIn(keyringPathDirs, "dbus-send"),
		DbusDaemon: lookPathIn(keyringPathDirs, "dbus-daemon"),
		Bwrap:      lookPathIn(keyringPathDirs, "bwrap"),
	}
	if c.Daemon == "" {
		c.Missing = append(c.Missing, "gnome-keyring-daemon")
	}
	if c.DbusSend == "" {
		c.Missing = append(c.Missing, "dbus-send")
	}
	c.Available = len(c.Missing) == 0
	return c
}

// lookPathIn 只在给定目录里找可执行普通文件；不跟随 PATH、不执行任何东西。
func lookPathIn(dirs []string, name string) string {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		st, err := os.Stat(p)
		if err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// KeyringDefault 是"该应用是否默认开启独立密钥环"的唯一判据（方案 §6）：
// 只有 observed 且能力可用才默认开启；linked 只提示，永不默认开。
// 第二个返回值是告警键（能力缺失时 `keyring_unavailable`），没有告警为空。
func KeyringDefault(evidence string) (bool, string) {
	if evidence != KeyringEvidenceObserved {
		return false, ""
	}
	if !KeyringCapability().Available {
		return false, "keyring_unavailable"
	}
	return true, ""
}

// keyringDefault 是探测内部使用的版本：能力结论可被测试注入。
func (e *Env) keyringDefault(evidence string) (bool, string) {
	if evidence != KeyringEvidenceObserved {
		return false, ""
	}
	if !e.keyringCaps().Available {
		return false, "keyring_unavailable"
	}
	return true, ""
}

// keyringEvidence 汇总三类证据（方案 §6）。强证据 observed 优先：
//  1. 候选里出现 ClassKeyring 路径（overlay 写证据）；
//  2. 读追踪命中 keyring 路径；
//  3. sendmsg 字面量里出现 org.freedesktop.secrets。
//
// 合成条目（probeUnsupported 无条件追加的 kind=keyring）**不参与**判定：
// 它是"方案隔离不了 keyring"的说明，不是"这个应用用了 keyring"的证据。
func (e *Env) keyringEvidence(exec string, cands []Candidate, reads, hits []string) string {
	if e.Resolver != nil {
		for _, c := range cands {
			if c.Class == ClassKeyring {
				return KeyringEvidenceObserved
			}
		}
		for _, p := range reads {
			if e.Resolver.isKeyring(p) {
				return KeyringEvidenceObserved
			}
		}
	}
	for _, h := range hits {
		if strings.Contains(h, "org.freedesktop.secrets") {
			return KeyringEvidenceObserved
		}
	}
	if elfLinksSecret(exec) {
		return KeyringEvidenceLinked
	}
	return KeyringEvidenceNone
}

// systemBinRoots 是"扫了也没有意义"的系统 bin 目录：里面的程序是发行版
// 打包的通用二进制，不构成"这个应用用了 libsecret"的证据。
var systemBinRoots = map[string]bool{
	"/usr/bin": true, "/usr/sbin": true, "/bin": true, "/sbin": true,
	"/usr/local/bin": true, "/usr/local/sbin": true,
}

const (
	elfScanDepth = 4
	elfScanFiles = 500
	elfScanTime  = 2 * time.Second
)

// elfLinksSecret 做零执行的 ELF 扫描：在 Exec 的真实安装目录里找链接了
// libsecret-1.so 的二进制。只读 ELF 头/动态段，不执行、不跟随目录外软链。
// Exec 是 shell wrapper（非 ELF）时不做目录扫描——扫描 sh 所在的 /bin 没有
// 任何信息量，靠 observed 或用户手动开启。
func elfLinksSecret(exec string) bool {
	if exec == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(exec)
	if err != nil {
		real = exec
	}
	real = filepath.Clean(real)
	root := filepath.Dir(real)
	if systemBinRoots[root] {
		return false
	}
	// 入口本身不是 ELF（shell/python wrapper）就不猜它装了谁：扫同目录只会
	// 把"同目录里别的程序用了 libsecret"误判成本应用。
	if !isELF(real) {
		return false
	}
	deadline := time.Now().Add(elfScanTime)
	files := 0
	found := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if time.Now().After(deadline) || files >= elfScanFiles || found {
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		depth := 0
		if rel != "." {
			depth = len(strings.Split(rel, string(filepath.Separator)))
		}
		if d.IsDir() {
			if depth > elfScanDepth {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files++
		if linksSecret(path) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// isELF 只读前 4 字节判断魔数。
func isELF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.Read(magic[:]); err != nil {
		return false
	}
	return string(magic[:]) == "\x7fELF"
}

// linksSecret 读 ELF 动态段，判断是否直接链接 libsecret。
func linksSecret(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	libs, err := f.ImportedLibraries()
	if err != nil {
		return false
	}
	for _, l := range libs {
		if strings.HasPrefix(l, "libsecret-1.so") {
			return true
		}
	}
	return false
}
