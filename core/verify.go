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
func (v Verdict) ExitCode() int {
	if v.Conclusion == VerdictLeak {
		return CodeLeak
	}
	return CodeOK
}

// AuditDir 返回实例的审计目录（证据元数据 + upper）。
func (e *Env) AuditDir(app, id string) string {
	return filepath.Join(e.InstancePath(app, id), "audit")
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
	// 档案里没写 ignored 时补默认值：不给默认噪声白名单，任何真实应用都会"泄漏"
	// （正常启动就写 ~/.cache），结论会变成噪声而不是信号（§5）。
	if len(p.Ignored) == 0 {
		p.Ignored = DefaultIgnored()
	}

	audit := e.AuditDir(app, id)
	entries, present, err := readUpperTree(filepath.Join(audit, "upper"))
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
	// 预期可写范围。
	var allowed []string
	add := func(list []string) {
		for _, raw := range list {
			if abs, err := e.Resolver.Expand(raw); err == nil {
				allowed = append(allowed, abs)
			}
		}
	}
	add(p.Ignored)
	add(p.SharedWrites)
	if e.Base != "" {
		allowed = append(allowed, e.Base)
	}

	// 先收候选越界项（绝对路径 → 去重）。
	seen := map[string]bool{}
	var cand []string
	for _, en := range entries {
		abs := filepath.Clean(filepath.Join(e.Resolver.Home, filepath.FromSlash(en.Path)))
		if !IsUnder(e.Resolver.Home, abs) || seen[abs] {
			continue
		}
		// 公共目录的优先级最高，**压过 shared_writes**：审计层把这些目录 bind 回
		// 真实位置，它们出现在 upper 里就说明摘出失效——那是第一红线，不许被
		// 档案里的 shared_writes 洗白（§6.5 准入条件）。
		if !e.Resolver.isPublic(abs) {
			if underAny(abs, allowed...) {
				continue
			}
			// 父目录是被子项"顺带"造出来的（`.config` 只因 `.config/X/y` 存在），
			// 它本身不是一次写入，报出来只会淹没真正的那条。
			if ancestorOfAny(abs, entries, e.Resolver.Home) {
				continue
			}
		}
		seen[abs] = true
		cand = append(cand, abs)
	}

	// cand 已经是最深的那批：父项在上一轮就被剔掉了，用户看到的正是
	// "哪里真的写了文件"（也是从 quarantine 里找回文件的位置）。
	// cand 的顺序是 walk 顺序，下面统一排序。
	v := Verdict{
		Conclusion: VerdictVerified,
		Coverage:   []string{e.Resolver.Home},
		Violations: append([]string{}, cand...),
		Ignored:    p.Ignored,
	}
	if len(v.Violations) > 0 {
		sort.Strings(v.Violations)
		v.Conclusion = VerdictLeak
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
