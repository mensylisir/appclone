package core

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"
)

// verify 的三种结论（§6.5）。`unverified` 是"不知道"，**不是通过**：
// 没有证据时把它显示成绿色，用户会以为隔离被验证过——这是本产品最不该犯的错。
const (
	VerdictVerified   = "verified"
	VerdictLeak       = "leak-detected"
	VerdictUnverified = "unverified"
)

// Verdict 是 `doctor --verify` 的完整结论。字段全是"给人看 + 给脚本用"的：
// 结论、覆盖率、越界清单、被忽略的类别、证据是否保留。
type Verdict struct {
	Conclusion string   `json:"conclusion"`
	Coverage   []string `json:"coverage"`
	Violations []string `json:"violations"`
	Ignored    []string `json:"ignored"`
	Retained   bool     `json:"retained"`         // 越界时证据必须留着，否则等于销毁用户数据
	Reason     string   `json:"reason,omitempty"` // unverified 的原因，如 audit_not_enabled
}

// ExitCode 是结论对应的退出码：只有 leak-detected 是失败（8）。
// verified 与 unverified 都是 0——后者是"不知道"，不是"出错"（§6.3）。
// 例外是 `extract_failed`：公共目录/AC_BASE/shared_writes 出现在 upper 里，
// 说明审计层的摘出没生效，这次审计本身作废（退出 4，§6.5 反向断言）。
func (v Verdict) ExitCode() int {
	switch {
	case v.Conclusion == VerdictLeak:
		return CodeLeak
	case v.Reason == "extract_failed":
		return CodeProbe
	}
	return CodeOK
}

// AuditDir 返回实例的审计目录：**只放轻量证据元数据**
// （run.toml / summary.toml / quarantine-*），不含 upper。
func (e *Env) AuditDir(app, id string) string {
	return filepath.Join(e.InstancePath(app, id), "audit")
}

// AuditWorkDir 是审计 overlay 的写层位置（§4.4/§6.5）：`upper/`、`work/`、`merged/`
// 必须在有效 HOME **之外**——overlay 的 upper 落在它自己的 lower 里会 ELOOP，
// 而且 AC_BASE 默认就在 `$HOME` 内。所以这里落在 `/var/tmp`，与实例目录分开。
func (e *Env) AuditWorkDir(app, id string) string {
	return filepath.Join("/var/tmp", "appclone", fmt.Sprintf("audit.%d", e.UID), app+"."+id)
}

// upperDir 定位要读的 upper：优先标准位置（审计写层），回退到实例内的
// `audit/upper`（写入器落地前，手工排障与测试把证据放这里）。
func (e *Env) upperDir(app, id string) string {
	work := filepath.Join(e.AuditWorkDir(app, id), "upper")
	if _, err := os.Stat(work); err == nil {
		return work
	}
	return filepath.Join(e.AuditDir(app, id), "upper")
}

// Verify 判定"这个分身有没有写到实例目录之外"（§6.5）。
//
// 能力边界必须说清：本版本没有审计写入器（`run --audit` 未实现），所以正常情况下
// `audit/upper` 不存在，结论只能是 `unverified` + `reason=audit_not_enabled`。
// 但判定逻辑本身是完整的、可测的——喂给它一棵 upper 树，它就能给出 verified /
// leak-detected。等审计层落地时，缺的只是一个"生产者"。
func (e *Env) Verify(app, id string) (Verdict, error) {
	in, err := e.GetInstance(app, id)
	if err != nil {
		return Verdict{}, err
	}
	// 运行中读到的 upper 是半截证据：对账对到一半的目录树，结论只能是错的。
	// 先 LOCK_NB 拿 run.lock，拿不到报 7。**不带 O_CREATE**：verify 是只读命令。
	lockPath := filepath.Join(in.Dir(), "run.lock")
	if f, lerr := os.OpenFile(lockPath, os.O_RDWR, 0o600); lerr == nil {
		if ferr := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); ferr != nil {
			f.Close()
			return Verdict{}, errState("err.instance.running", map[string]any{"app": app, "id": id})
		}
		defer func() {
			unix.Flock(int(f.Fd()), unix.LOCK_UN)
			f.Close()
		}()
	} else if !os.IsNotExist(lerr) {
		return Verdict{}, errEnv("err.env.open_lock", map[string]any{"path": lockPath}, lerr)
	}

	p, err := e.LoadProfile(app)
	if err != nil {
		return Verdict{}, err
	}
	// 档案里"没写 ignored"时补默认值：不给默认噪声白名单，任何真实应用都会
	// "泄漏"（正常启动就写 ~/.cache），结论会变成噪声而不是信号（§5）。
	//
	// 只补**缺字段**，不碰显式空数组：`ignored = []` 是用户关掉噪声白名单的
	// 选择，把它覆盖成默认值等于静默推翻用户意图（§5.5"缺字段用默认值，
	// 非法值报错"——在这里 nil 才是"没写"）。
	if p.Ignored == nil {
		p.Ignored = DefaultIgnored()
	}

	audit := e.AuditDir(app, id)
	entries, present, err := readUpperTree(e.upperDir(app, id))
	if err != nil {
		return Verdict{}, err
	}
	runInfo := fileExists(filepath.Join(audit, "run.toml"))
	summary, hasSummary, err := readSummary(filepath.Join(audit, "summary.toml"))
	if err != nil {
		return Verdict{}, err
	}

	unverified := func(reason string, retained bool) Verdict {
		return Verdict{
			Conclusion: VerdictUnverified,
			Coverage:   []string{e.Resolver.Home},
			Violations: []string{},
			Ignored:    p.Ignored,
			Reason:     reason,
			Retained:   retained,
		}
	}

	switch {
	case !present && hasSummary:
		// upper 已被消费过：**重放**已存结论，不重新对账（§6.5 表格第 4 行）。
		// 重放时也不改 Retained：摘要里写着证据留没留。
		summary.Ignored = p.Ignored
		return summary, nil
	case !present && runInfo:
		// 有 run.toml 却没有 upper：upper 被误删或审计层中途崩了。
		// 这是"证据不完整"，不是"从没审计"——两者的下一步完全不同。
		return unverified("audit_incomplete", true), nil
	case !present:
		// 从没审计过。这是当前版本的常态：没有审计写入器（run --audit 未实现）。
		return unverified("audit_not_enabled", false), nil
	case !runInfo:
		// 有 upper 却没有 run.toml：上次 run 崩了或被 kill，证据不完整。
		// 这种情况**绝不能**说 verified，也不许删 upper（§6.5 表格第 3 行）。
		return unverified("audit_incomplete", true), nil
	case len(entries) == 0:
		// 审计跑了、应用什么都没写。空集不算通过：可能是应用根本没启动成功，
		// 也可能是审计层没挂上——两种都不足以证明隔离有效（§6.5 表格第 5 行）。
		return unverified("no_writes", false), nil
	}
	return e.Judge(p, entries), nil
}

// Summary 是 `audit/summary.toml` 的形状：几 KB 的结论摘要，upper 被消费后
// 它就是唯一的证据（§6.5）。
type Summary struct {
	Conclusion  string   `toml:"conclusion"`
	Coverage    []string `toml:"coverage"`
	Violations  []string `toml:"violations"`
	Retained    bool     `toml:"evidence_retained"`
	GeneratedAt string   `toml:"generated_at,omitempty"`
	Reason      string   `toml:"reason,omitempty"`
	Ignored     []string `toml:"-"`
}

func (s Summary) Verdict() Verdict {
	return Verdict{
		Conclusion: s.Conclusion,
		Coverage:   s.Coverage,
		Violations: s.Violations,
		Ignored:    s.Ignored,
		Retained:   s.Retained,
		Reason:     s.Reason,
	}
}

// readSummary 读已存结论。文件不存在返回 has=false；解析失败按"证据不可读"
// 处理成 unverified，绝不因为读不出来就说通过。
func readSummary(path string) (Verdict, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Verdict{}, false, nil
		}
		return Verdict{}, false, errEnv("err.internal.read_profile", map[string]any{"path": path}, err)
	}
	var s Summary
	if _, err := toml.Decode(string(data), &s); err != nil || s.Conclusion == "" {
		return Verdict{
			Conclusion: VerdictUnverified,
			Coverage:   []string{},
			Violations: []string{},
			Ignored:    []string{},
			Reason:     "audit_unreadable",
			Retained:   true, // 读不出来就不知道里面有什么，不能当它不存在
		}, true, nil
	}
	if s.Coverage == nil {
		s.Coverage = []string{}
	}
	if s.Violations == nil {
		s.Violations = []string{}
	}
	return s.Verdict(), true, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Judge 把一棵 upper 树判成结论。**纯函数**，不读盘、不删任何东西——
// 判定与"删不删证据"必须分开：判定错会误导用户，删错会毁掉用户的文件。
//
// 归因口径：upper 里出现的路径只要落在"预期会写"的范围里就不算越界——
// 忽略类别（`~/.cache` 等噪声）、`shared_writes`（本来就共享）、公共目录
// （摘出后不该出现，出现了说明摘出失效，那本身是泄漏，所以**不**在白名单里，
// 见下）、`AC_BASE`（工具自己的写入）。
func (e *Env) Judge(p Profile, entries []UpperEntry) Verdict {
	// 三档范围，优先级即顺序（§6.5）：
	//  ① 摘出失效档：公共目录 / AC_BASE / shared_writes 本该被 bind 回真实路径，
	//    它们出现在 upper 里就说明摘出没生效——此时任何结论都不算数（退出 4）。
	//    这一档必须排在对账之前：否则摘出失效会被当成"预期共享"，而下一轮
	//    "verified 才删 upper" 就会把用户文件一起删掉。
	//  ② 通过档：paths（已隔离，写进去也等于隔离生效）、unsupported（不在
	//    文件路径上，用路径判定没有意义）、ignored（噪声类别，本来就该进 upper）。
	//  ③ 其余 → 越界。
	var mustExtract, allowed []string
	expandInto := func(dst *[]string, list []string) {
		for _, raw := range list {
			if abs, err := e.Resolver.Expand(raw); err == nil {
				*dst = append(*dst, abs)
			}
		}
	}
	expandInto(&mustExtract, p.SharedWrites)
	for _, pub := range e.Resolver.PublicDirs() {
		mustExtract = append(mustExtract, pub)
	}
	if e.Base != "" {
		mustExtract = append(mustExtract, e.Base)
	}
	expandInto(&allowed, p.Paths)
	expandInto(&allowed, p.Ignored)
	for _, u := range p.Unsupported {
		if u.Path != "" {
			expandInto(&allowed, []string{u.Path})
		}
	}

	v := Verdict{
		Conclusion: VerdictVerified,
		Coverage:   []string{e.Resolver.Home},
		Violations: []string{},
		Ignored:    p.Ignored,
	}
	seen := map[string]bool{}
	var extractFailed []string
	var leaked []string
	for _, en := range entries {
		abs := filepath.Clean(filepath.Join(e.Resolver.Home, filepath.FromSlash(en.Path)))
		if !IsUnder(e.Resolver.Home, abs) || seen[abs] {
			continue
		}
		seen[abs] = true
		// 父目录是被子项"顺带"造出来的（`.config` 只因 `.config/X/y` 存在），
		// 它本身不是一次写入，报出来只会淹没真正的那条。
		isAncestor := ancestorOfAny(abs, entries, e.Resolver.Home)
		if underAny(abs, mustExtract...) {
			if !isAncestor {
				extractFailed = append(extractFailed, abs)
			}
			continue
		}
		if underAny(abs, allowed...) {
			continue
		}
		if isAncestor {
			continue
		}
		leaked = append(leaked, abs)
	}

	if len(extractFailed) > 0 {
		sort.Strings(extractFailed)
		return Verdict{
			Conclusion: VerdictUnverified,
			Coverage:   []string{e.Resolver.Home},
			Violations: extractFailed,
			Ignored:    p.Ignored,
			Retained:   true,
			Reason:     "extract_failed",
		}
	}
	if len(leaked) > 0 {
		sort.Strings(leaked)
		v.Conclusion = VerdictLeak
		v.Violations = leaked
		// 越界文件就在 upper 里：此刻删证据等于销毁用户数据。
		v.Retained = true
	}
	return v
}

// ancestorOfAny 判定 abs 是否是 entries 里某条**更深**条目的祖先。
func ancestorOfAny(abs string, entries []UpperEntry, home string) bool {
	for _, en := range entries {
		other := filepath.Clean(filepath.Join(home, filepath.FromSlash(en.Path)))
		if other != abs && IsUnder(abs, other) {
			return true
		}
	}
	return false
}

// readUpperTree 把 audit/upper 读成条目列表。present=false 表示"没有证据"，
// 它与"读了但为空"是两件事：后者意味着审计跑过且**什么都没写**，前者意味着
// 根本没审计——结论不同（unverified vs verified），不能合并。
func readUpperTree(dir string) (entries []UpperEntry, present bool, err error) {
	fi, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, errEnv("err.env.read_dir", map[string]any{"path": dir}, err)
	}
	if !fi.IsDir() {
		return nil, false, errEnv("err.env.read_dir", map[string]any{"path": dir}, nil)
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if path == dir {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil
		}
		kind := EntryFile
		switch {
		case d.IsDir():
			kind = EntryDir
		case strings.HasPrefix(d.Name(), ".wh."):
			kind = EntryWhiteout
			// `.wh.foo` 指的是 `foo` 被删了：去掉前缀再记录，否则对账会把
			// 一个叫 `.wh.foo` 的"路径"报成越界，恢复提示也会指向不存在的文件。
			rel = filepath.Join(filepath.Dir(rel), strings.TrimPrefix(d.Name(), ".wh."))
		case d.Type()&fs.ModeDevice != 0 && d.Type()&fs.ModeCharDevice != 0:
			kind = EntryWhiteout // overlayfs 的 whiteout：字符设备 0,0
		}
		var size int64
		if info, ierr := d.Info(); ierr == nil {
			size = info.Size()
		}
		entries = append(entries, UpperEntry{Path: filepath.ToSlash(rel), Kind: kind, Size: size})
		return nil
	})
	if err != nil {
		return nil, false, errEnv("err.env.read_dir", map[string]any{"path": dir}, err)
	}
	return entries, true, nil
}

// 便于视图层拼一句话：结论 + 覆盖范围。人类文案在视图里组装，这里只给事实。
func (v Verdict) String() string {
	return fmt.Sprintf("verify(%s, violations=%d)", v.Conclusion, len(v.Violations))
}
