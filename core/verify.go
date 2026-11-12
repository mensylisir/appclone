package core

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Conclusion  string   `json:"conclusion"`
	Coverage    []string `json:"coverage"`
	Violations  []string `json:"violations"`
	Ignored     []string `json:"ignored"`
	Retained    bool     `json:"retained"`               // 越界时证据必须留着，否则等于销毁用户数据
	Reason      string   `json:"reason,omitempty"`       // unverified 的原因，如 audit_not_enabled
	Warnings    []string `json:"warnings,omitempty"`     // 结论有效但需要提示的异常（如 masked_base_missing）
	GeneratedAt string   `json:"generated_at,omitempty"` // 重放已存摘要时带上它，用户才知道结论有多旧
	Quarantine  string   `json:"quarantine,omitempty"`   // 越界证据被搬到哪里（空 = 没有搬）
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

// AuditDir 返回实例的审计目录：放 `run.toml` / `summary.toml` 这类凭证，
// 以及作废时搬进来的 `quarantine-*`（**里面可能装着用户文件，永不自动删**）。
// overlay 的 upper/work/merged 不在这里——它们在写层根 /tmp（§4.4/§6.5，ADR-033）。
func (e *Env) AuditDir(app, id string) string {
	return filepath.Join(e.InstancePath(app, id), "audit")
}

// CachedAuditVerdict 读上一次 verify 写下的结论，**只读、不消费证据**。
// GUI 详情页需要显示"最近审计"，但它不能调用 Verify：那会拿 run.lock、
// 甚至 finalize 掉证据，等于看一眼就把证据吃掉。
//
// reason 一起返回：`unverified` 不是一种状态而是四种（没开审计 / 证据没写完 /
// 确实没观测到写入 / 证据读不出来）。只把结论交给界面，就等于把"证据读不出"
// 显示成"没结论"——用户据此会去重跑而不是去修文件。
func (e *Env) CachedAuditVerdict(app, id string) (conclusion, reason, generatedAt string, ok bool) {
	s, has, err := readSummary(filepath.Join(e.AuditDir(app, id), "summary.toml"))
	if err != nil || !has {
		return "", "", "", false
	}
	return s.Conclusion, s.Reason, s.GeneratedAt, true
}

// latestQuarantine 返回实例 audit/ 下最近的一份 quarantine-*。
// 名字里带 UTC 时间戳，字典序即时间序。没有则返回空串。
func (e *Env) latestQuarantine(app, id string) string {
	dirs, _ := filepath.Glob(filepath.Join(e.AuditDir(app, id), "quarantine-*"))
	if len(dirs) == 0 {
		return ""
	}
	sort.Strings(dirs)
	return dirs[len(dirs)-1]
}

// AuditWorkDir 是审计 overlay 的写层位置（§4.4/§6.5）：`upper/`、`work/`、`merged/`
// 必须在有效 HOME **之外**——overlay 的 upper 落在它自己的 lower 里会 ELOOP，
// 而且 AC_BASE 默认就在 `$HOME` 内。所以这里落在写层根 `/tmp`（ADR-033），与实例目录分开。
func (e *Env) AuditWorkDir(app, id string) string {
	return filepath.Join(e.tmpRoot(), "appclone", fmt.Sprintf("audit.%d", e.UID), app+"."+id)
}

// upperDir 定位要读的 upper：优先新写层根，其次旧根 /var/tmp（ADR-033
// 升级路径），最后回退到实例内的 `audit/upper`（写入器落地前，手工排障与
// 测试把证据放这里）。
func (e *Env) upperDir(app, id string) string {
	work := filepath.Join(e.AuditWorkDir(app, id), "upper")
	if _, err := os.Stat(work); err == nil {
		return work
	}
	// 升级路径（ADR-033）：旧版本的审计写层在 /var/tmp。若那边还留着已跑完、
	// 尚未 verify 的证据，必须能继续消费——否则 verify 会判"证据不完整"，
	// 用户以为审计结论丢了。新根优先，旧根只在新根没有时作为回退。
	if e.TmpRoot == "" {
		legacy := filepath.Join(legacyTmpRoot, "appclone", fmt.Sprintf("audit.%d", e.UID), app+"."+id, "upper")
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return filepath.Join(e.AuditDir(app, id), "upper")
}

// Verify 判定"这个分身有没有写到实例目录之外"（§6.5）。
//
// 证据生产者是 `run --audit`（探针写层）；没有审计过的实例读不到 upper，
// 结论只能是 `unverified` + `reason=audit_not_enabled`——**不是通过**。
// 判定逻辑本身是纯的、可测的：喂给它一棵 upper 树，它就能给出 verified /
// leak-detected。
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
	// 判定之前必须过一遍档案校验：`ignored`/`shared_writes`/`unsupported` 是
	// verify 的放行名单，手改（或旧版本残留）一个 `~` 进去，Judge 就会对任何
	// 越界都说 verified。只在 add/run 的入口校验、不在 verify 校验，等于把
	// 这条最危险假通过的防线放在用户根本不会再走一遍的地方。
	if err := ValidateProfile(p, e.BuildOptions()); err != nil {
		return Verdict{}, err
	}

	audit := e.AuditDir(app, id)
	entries, present, err := readUpperTree(e.upperDir(app, id))
	if err != nil {
		return Verdict{}, err
	}
	rec, runInfo, err := readRunRecord(filepath.Join(audit, "run.toml"))
	if err != nil {
		return Verdict{}, err
	}
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
	case !present && runInfo && !rec.complete():
		// 新一轮审计已经开过，但没正常结束（连写层都没建起来就失败，或早期
		// 被杀）。此时目录里可能还有上一轮的 summary.toml——那是**更早一次**
		// 审计的结论，不能重放成"这次通过"。没有证据就是没有证据。
		return unverified("audit_incomplete", false), nil
	case present && runInfo && !rec.complete():
		// 写层在、凭证没写完：会话中途崩了。证据保留，但不下结论。
		return unverified("audit_incomplete", true), nil
	case !present && hasSummary:
		// upper 已被消费过：**重放**已存结论，不重新对账（§6.5 表格第 4 行）。
		// 重放时也不改 Retained：摘要里写着证据留没留。走到这里说明没有更新的、
		// 未完成的审计尝试（上面两条已经拦掉了）。
		//
		// ignored 优先用摘要里的：那是**审计当时**的忽略清单，用现在的档案覆盖它
		// 等于篡改证据（用户可能已经改过档案）。
		if len(summary.Ignored) == 0 {
			summary.Ignored = p.Ignored
		}
		return summary, nil
	case !present && runInfo:
		// 有 run.toml 却没有 upper：upper 被误删或审计层中途崩了。
		// 这是"证据不完整"，不是"从没审计"——两者的下一步完全不同。
		v := unverified("audit_incomplete", true)
		// 被前一轮 verify 或"重开审计"搬进 quarantine-* 的证据，把最新那份的
		// 真实路径带出去。只在 upper 确实不在时才指 quarantine——否则现场就在
		// 工作区里，指向旧 quarantine 会把用户引到一份无关的证据上。
		v.Quarantine = e.latestQuarantine(app, id)
		return v, nil
	case !present:
		// 从没审计过。这是当前版本的常态：没有审计写入器（run --audit 未实现）。
		return unverified("audit_not_enabled", false), nil
	case !runInfo:
		// 有 upper 却没有 run.toml：上次 run 崩了或被 kill，证据不完整。
		// 这种情况**绝不能**说 verified，也不许删 upper（§6.5 表格第 3 行）。
		return unverified("audit_incomplete", true), nil
	case rec.Version == 0:
		// run.toml 存在但解析不了（写坏/被截断）：凭证不完整，不能拿来对账。
		// 降级成 audit_incomplete 而不是环境错误——用户还能重跑一次审计。
		return unverified("audit_incomplete", true), nil
	case rec.Version > ProfileVersion:
		// 未来版本的写入器留下的凭证：字段语义可能已经变了，按 v1 解释未知
		// 格式等同于猜测。保留证据、不下结论，等用户升级后再 verify。
		return unverified("audit_incomplete", true), nil
	case rec.App != app || rec.ID != id:
		// 凭证不是这个实例的（目录被手工搬过/复制过）：拿来对账会得出别人的结论。
		return unverified("audit_incomplete", true), nil
	case len(entries) == 0:
		// 审计跑了、应用什么都没写。空集不算通过：可能是应用根本没启动成功，
		// 也可能是审计层没挂上——两种都不足以证明隔离有效（§6.5 表格第 5 行）。
		return unverified("no_writes", false), nil
	}
	return e.finalize(app, id, p, e.judge(p, entries, &rec))
}

// Judge 是 judge 的无凭证版本：没有 run.toml 时按档案推断"本该被 bind"的集合。
// 生产路径走 Verify（带 run.toml 的实际 bind 清单），这里主要给单测与排障用。
func (e *Env) Judge(p Profile, entries []UpperEntry) Verdict {
	return e.judge(p, entries, nil)
}

// finalize 把结论落账，并按结论处置证据（§6.5 留存策略）。**判**与**处置**分开：
// 判定错会误导用户，处置错会毁掉用户的文件。
//
//   - verified      → 写 summary.toml，删 upper 与 run.toml（证据已消费）
//   - leak-detected → 写 summary.toml（retained），upper **搬进** quarantine-*
//     （不删：越界文件就在里面，删掉等于销毁用户数据）
//   - extract_failed→ 与 leak 相同：搬进 quarantine。现场以**唯一命名**保留，
//     新审计不会覆盖它，所以不必再把"有 retained 证据"当成不许开新审计的理由
//     ——那会让用户取回文件后永远无法再审计（旧实现正是如此）。
//   - 其余 unverified（无证据/证据不完整/空 upper）→ 不写摘要、不动现场
func (e *Env) finalize(app, id string, p Profile, v Verdict) (Verdict, error) {
	if v.Conclusion == VerdictUnverified && v.Reason != "extract_failed" {
		return v, nil
	}
	audit := e.AuditDir(app, id)
	if err := os.MkdirAll(audit, 0o700); err != nil {
		return Verdict{}, errEnv("err.env.mkdir", map[string]any{"path": audit}, err)
	}

	// 处置顺序：先把证据搬走/删掉，再写摘要——反过来一旦摘要写成功、
	// 处置失败，下一次 verify 会把"已消费"当成结论，现场就再也找不回了。
	//
	// 搬运用 moveDir 而不是 os.Rename：写层在 /tmp，实例目录在 $HOME，
	// 两者常常不在同一个文件系统上（/home 独立分区是普通桌面配置），
	// rename 会 EXDEV 直接失败，leak 结论永远落不了地。
	switch {
	case v.Conclusion == VerdictLeak || v.Reason == "extract_failed":
		q := filepath.Join(audit, "quarantine-"+time.Now().UTC().Format("20060102T150405")+"-"+randSuffix())
		if err := moveDir(e.upperDir(app, id), q); err != nil && !dstComplete(err) {
			// 副本完整、只是源删不干净时按成功处理：证据在 q，别报失败把
			// 用户引向残缺的另一份；残留由下次清扫收拾。
			return Verdict{}, errEnv("err.probe.quarantine_failed", map[string]any{"path": q}, err)
		}
		e.removeAuditWork(app, id)
		v.Retained = true
		v.Quarantine = q
	case v.Conclusion == VerdictVerified:
		// upper 里只有声明范围内的写入，删掉是对的；但应用可能把某个子目录
		// 改成 0555，裸 RemoveAll 会让一个已验证的结论无法收尾。
		if err := makeWritableAndRemove(e.upperDir(app, id)); err != nil {
			return Verdict{}, errEnv("err.env.write", map[string]any{"path": e.upperDir(app, id)}, err)
		}
		e.removeAuditWork(app, id)
		_ = os.Remove(filepath.Join(audit, "run.toml"))
	}

	sum := Summary{
		Conclusion:  v.Conclusion,
		Coverage:    v.Coverage,
		Violations:  v.Violations,
		Ignored:     v.Ignored,
		Retained:    v.Retained,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Reason:      v.Reason,
		Warnings:    v.Warnings,
		Quarantine:  v.Quarantine,
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(sum); err != nil {
		return Verdict{}, errInternal("err.internal.encode_profile", nil, err)
	}
	// 原子写：半份摘要比没有摘要更坏（下次会按"损坏"处理，把结论降级成不可读）。
	if err := writeFileAtomic(filepath.Join(audit, "summary.toml"), buf.Bytes(), 0o600); err != nil {
		// 证据已经搬进 quarantine-*，摘要却没写成功：错误信息必须带上那个
		// 路径，否则用户/下一次 verify 再也找不到这份现场（它没有指针）。
		if v.Quarantine != "" {
			return Verdict{}, errEnv("err.audit.summary_write", map[string]any{
				"path": filepath.Join(audit, "summary.toml"), "quarantine": v.Quarantine,
			}, err)
		}
		return Verdict{}, err
	}
	// 不把"刚刚"写回 Verdict.GeneratedAt：CLI 用它标注"重放的已存摘要"，
	// 刚判出来的结论带着它会被误标成重放。时间戳以摘要文件为准。
	return v, nil
}

// Summary 是 `audit/summary.toml` 的形状：几 KB 的结论摘要，upper 被消费后
// 它就是唯一的证据（§6.5）。
type Summary struct {
	Conclusion  string   `toml:"conclusion"`
	Coverage    []string `toml:"coverage"`
	Violations  []string `toml:"violations"`
	Ignored     []string `toml:"ignored,omitempty"` // §5：结论必须带上"被忽略的类别"
	Retained    bool     `toml:"evidence_retained"`
	GeneratedAt string   `toml:"generated_at,omitempty"`
	Reason      string   `toml:"reason,omitempty"`
	// Warnings 与 Verdict.Warnings 同义：重放时不能丢（masked_base_missing
	// 只在新跑时出现、重放就消失，等于把异常藏起来）。
	Warnings []string `toml:"warnings,omitempty"`
	// Quarantine 是证据被搬到的位置（绝对路径）。重放时用户必须能据此找回文件，
	// 否则摘要只告诉他"泄漏了"，却不说东西在哪。
	Quarantine string `toml:"quarantine,omitempty"`
}

func (s Summary) Verdict() Verdict {
	return Verdict{
		Conclusion:  s.Conclusion,
		Coverage:    s.Coverage,
		Violations:  s.Violations,
		Ignored:     s.Ignored,
		Retained:    s.Retained,
		Reason:      s.Reason,
		Warnings:    s.Warnings,
		GeneratedAt: s.GeneratedAt,
		Quarantine:  s.Quarantine,
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
	// 结论必须是枚举里的三选一：手写 `conclusion = "OK"` 会被 ExitCode 当成 0
	// （"通过"），而 CLI 的 switch 落到 default 打印 unverified —— 同一份证据两种说法。
	// reason 同理：它决定 CLI 说"哪种不知道"，未知值会落到错误的下一步提示。
	if _, derr := toml.Decode(string(data), &s); derr != nil || !isVerdict(s.Conclusion) ||
		(s.Conclusion == VerdictUnverified && !isKnownReason(s.Reason)) {
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

func isVerdict(s string) bool {
	return s == VerdictVerified || s == VerdictLeak || s == VerdictUnverified
}

// isKnownReason 是 unverified 的合法原因集。空串也合法（老摘要没写 reason）。
func isKnownReason(s string) bool {
	switch s {
	case "", "audit_not_enabled", "audit_incomplete", "no_writes", "audit_unreadable", "extract_failed":
		return true
	}
	return false
}

// judge 把一棵 upper 树判成结论。**纯函数**，不读盘、不删任何东西——
// 判定与"删不删证据"必须分开：判定错会误导用户，删错会毁掉用户的文件。
//
// 归因口径：upper 里出现的路径只要落在"预期会写"的范围里就不算越界——
// 忽略类别（`~/.cache` 等噪声）、`shared_writes`（本来就共享）、公共目录
// （摘出后不该出现，出现了说明摘出失效，那本身是泄漏，所以**不**在白名单里，
// 见下）、`AC_BASE`（工具自己的写入）。
//
// rec 为本次审计 run.toml 里的实际 bind 清单；nil 时退回按档案推断（仅单测/排障）。
func (e *Env) judge(p Profile, entries []UpperEntry, rec *runRecord) Verdict {
	// 两档范围，优先级即顺序（§6.5）：
	//  ① 本该被 bind 走的路径：公共目录、AC_BASE、`shared_writes`、**`paths`**。
	//    审计层把它们 bind 回真实位置/实例目录，它们出现在 upper 里就说明 bind
	//    没生效——此时任何结论都不算数（`extract_failed`，退出 4，保留现场）。
	//    `paths` 必须在这一档：应用数据落进 upper 正是"实例 bind 被公共目录 bind
	//    盖掉"的症状，把它算通过等于把 bind 失败静默掉。
	//  ② 通过档：`ignored`（噪声类别，本来就该进 upper）、`unsupported`（不在
	//    文件路径上，用路径判定没有意义）。
	//  ③ 其余 → 越界。
	var mustBind, allowed []string
	// 新记录缺 masked_base（写入器异常或手改）时仍按 Base 判 extract_failed，
	// 但要让用户看见"这份证据不完整"。
	maskedMissing := false
	expandInto := func(dst *[]string, list []string) {
		for _, raw := range list {
			if abs, err := e.Resolver.Expand(raw); err == nil {
				*dst = append(*dst, abs)
			}
		}
	}
	// paths 一定被 bind（缺源时 run 会先失败），所以它总是判断依据。
	if rec != nil && rec.BindsRecorded {
		// 新格式 run.toml：用**审计当时**的 paths，而不是当前档案。审计之后
		// 用户重新探测让 paths 变大时，旧证据里"当时未 bind"的路径会被当前
		// 档案误判成 extract_failed。
		mustBind = append(mustBind, rec.Paths...)
		if len(rec.Paths) == 0 {
			// 写入器异常（所有 paths 都在 Expand 阶段失败）才会出现"新格式但
			// 清单为空"。此时按单测/损坏凭证处理：宁可多报 extract_failed
			// （方向是"作废后重审"），也不能因为清单缺失把真实摘出失效判成
			// leak-detected（方向是"重新探测"，完全错）。正常写入器不会走到。
			expandInto(&mustBind, p.Paths)
		}
		// 只用**实际** bind 过的清单。按档案里写了什么推断，会把"没配置 /
		// 当时不存在因此被 --bind-try 跳过"的目录判成摘出失效——那是一次
		// 凭空产生的 extract_failed，会让审计永远作废。
		mustBind = append(mustBind, rec.PublicBinds...)
		mustBind = append(mustBind, rec.SharedBinds...)
		// keyring 目标也必须实际 bind：目录下出现未 bind 的写入说明独立
		// 密钥环没生效（v1.0.3 起）。
		mustBind = append(mustBind, rec.KeyringBinds...)
		// AC_BASE 现在是 `--tmpfs` 遮蔽路径，不再是 bind：Base 下出现写入
		// 说明遮蔽没生效。新记录用 MaskedBase；异常缺字段时退回 Base，
		// 方向仍是 extract_failed（不能掉成普通 leak），并记一条 warning。
		if rec.MaskedBase != "" {
			mustBind = append(mustBind, rec.MaskedBase)
		} else {
			// 新格式却没有 masked_base：写入器异常或手改凭证。方向必须是
			// extract_failed（"作废后重审"），不能掉成 leak-detected（"重新
			// 探测"）。两者都空时用当前 e.Base 兜底，并始终给出 warning——
			// 静默会让用户回看摘要时看不到这次异常。
			maskedMissing = true
			switch {
			case rec.Base != "":
				mustBind = append(mustBind, rec.Base)
			case e.Base != "":
				mustBind = append(mustBind, e.Base)
			}
		}
	} else {
		// 老格式（没有 binds_recorded）或单测：退回按档案推断。这条回退是
		// 兼容性的代价——老证据无法区分"没 bind"和"bind 失效"，至少方向
		// 不能错（extract_failed 与 leak-detected 的下一步完全相反）。
		expandInto(&mustBind, p.Paths)
		expandInto(&mustBind, p.SharedWrites)
		mustBind = append(mustBind, e.Resolver.PublicDirs()...)
		// 旧记录若写了 base，用它：AC_BASE 改过之后按当前 e.Base 重放会
		// 漏判旧审计里的 Base 写入。没写才退回当前值。
		if rec != nil && rec.Base != "" {
			mustBind = append(mustBind, rec.Base)
		} else if e.Base != "" {
			mustBind = append(mustBind, e.Base)
		}
	}
	// 白名单里的"家目录本身及其祖先"永远不能生效：那是全部放行，不是一条类别。
	// ValidateProfile 已经会拒绝这种档案，这里再做一次纵深防御——旧版本写下的
	// 档案、或将来某条路径绕过校验时，Judge 也不能把边界交出去。
	expandAllowed := func(list []string) {
		for _, raw := range list {
			if abs, err := e.Resolver.Expand(raw); err == nil && !e.blanketPath(abs) {
				allowed = append(allowed, abs)
			}
		}
	}
	expandAllowed(p.Ignored)
	// 声明过、但审计开始时不存在因此没被 `--bind-try` 绑上的 shared_writes：
	// 应用在审计期间把它建出来会落在 upper 里。那仍属于"用户声明共享"的路径，
	// 不是越界——判成 leak 给出的下一步（重新探测）是错的。
	expandAllowed(p.SharedWrites)
	for _, u := range p.Unsupported {
		if u.Path != "" {
			expandAllowed([]string{u.Path})
		}
	}

	v := Verdict{
		Conclusion: VerdictVerified,
		Coverage:   []string{e.Resolver.Home},
		Violations: []string{},
		Ignored:    p.Ignored,
	}
	if maskedMissing {
		v.Warnings = append(v.Warnings, "masked_base_missing")
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
		if underAny(abs, mustBind...) {
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
			Warnings:   v.Warnings,
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

// blanketPath 判定一个白名单条目是否等于 HOME 或它的祖先（= 放行整个主目录）。
// 比较用词法 + 真实两种形态：HOME 是软链时只比一种会漏判。
func (e *Env) blanketPath(abs string) bool {
	if abs == "/" {
		return true
	}
	for _, home := range []string{filepath.Clean(e.Resolver.Home), filepath.Clean(e.Resolver.RealHome)} {
		if home == "" || home == "." {
			continue
		}
		if abs == home || isUnderLoose(abs, home) {
			return true
		}
	}
	return false
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

// readUpperTree 把 upper 读成条目列表。present=false 表示"没有证据"，
// 它与"读了但为空"是两件事：后者意味着审计跑过且**什么都没写**，前者意味着
// 根本没审计。**两者都是 unverified**（空集不算通过，§6.5），但原因不同
// （`audit_not_enabled` vs `no_writes`），下一步也不同，所以不能合并。
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
		// overlayfs 的 opaque 标记（`.wh..wh..opq`）不是删除，也不是用户文件：
		// 它只表示"这个目录被整体替换过"。按 `.wh.` 前缀去前缀会变成不存在
		// 的 `.wh..opq`，再作为"缺失文件"展示给用户，把人引向一个假路径。
		if d.Name() == ".wh..wh..opq" {
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
