package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"
)

// AuditSession 是一次 `run --audit` 的现场（§6.5）。
//
// 与探测的区别只有两处，其余完全复用运行期那套：多一层 overlay（写层留证据）、
// 结束时不删写层而是写 `audit/run.toml`。**这两处正是"审计能给出结论"的全部前提**：
// 没有写层就没有证据，没有 run.toml 就无法判断"这次审计是不是正常跑完的"。
type AuditSession struct {
	e     *Env
	in    Instance
	p     Profile
	work  string // <tmpRoot>/appclone/audit.<uid>/<app>.<id>
	upper string // work/upper
	start time.Time
	// 本次审计**实际** bind 回真实的清单。verify 必须按它判 extract_failed，
	// 不能按"配置文件里写了什么"猜：没配置/当时不存在的公共目录根本没被 bind，
	// 写到它们不该算 bind 失效（§6.5 的 mustBind 口径必须与生产者一致）。
	pubs   []BindPair
	shared []string
	base   string
	// stop 是卸载 overlay 的闭包；finish/abort 之后置 nil，避免二次卸载。
	stop func() error
}

// beginAudit 准备审计会话并返回要插进 bwrap 参数的那一层。
//
// 前置（§6.5）：① 上一轮写层还有内容——先搬到唯一命名的 quarantine-*，既不丢
// 证据也不阻塞新审计（旧实现直接拒绝，用户取回文件后永远无法再审计）；
// ② 扫掉残留（含仍挂载的）；③ 写层必须在 $HOME 之外（overlay 的 upper 嵌在
// lower 里会 ELOOP）；④ 删掉上一轮的 run.toml——否则新审计中途崩掉时，
// verify 会把旧凭证当成本次"正常跑完"。
func (e *Env) beginAudit(in Instance, p Profile) (*AuditSession, *AuditLayers, error) {
	if err := e.quarantinePendingAudit(in); err != nil {
		return nil, nil, err
	}
	// 写层根在 AC_BASE 里时，`--tmpfs Base` 会把它一起盖住（应用看不到，
	// 工具自己也挂不上）；而且写层嵌在实例目录里等于把证据暴露给应用。
	writeRoot := filepath.Join(e.tmpRoot(), "appclone", fmt.Sprintf("audit.%d", e.UID))
	// 双向都拒：writeRoot 在 Base 下会被 `--tmpfs Base` 盖住；Base 在
	// writeRoot 下会被 `--tmpfs WriteRoot` 连带遮掉（实例 bind 目标在
	// 被遮的祖先下，审计会话起不来）。两边都取真实路径再比：tmpRoot
	// （/tmp、$XDG_RUNTIME_DIR）或 AC_BASE 可能是软链，词法比较会漏判。
	if base := Real(e.Base); base != "" {
		wr := Real(writeRoot)
		if wr == base || IsUnder(base, wr) || IsUnder(wr, base) {
			return nil, nil, errInput("err.profile.base_exposure", map[string]any{
				"path": writeRoot, "base": base, "field": "audit_write_root",
			})
		}
	}
	// 先用一条 finished_at 为空的"进行中"凭证顶掉上一轮 run.toml。
	//
	// 不能简单删除：如果这次审计在写层建起来之前就失败（mount/probePublicBinds/
	// Mkdir 失败，或早期被 SIGKILL），目录里就只剩旧 summary.toml；verify 一旦
	// 重放它，会把上一轮的 verified 当成"这次审计通过"（退出码 0）。写成凭证后，
	// verify 看到"有凭证、没跑完"会判 audit_incomplete——旧摘要不再被当成本次结论。
	//
	// 写入失败必须拦住新审计：否则状态退化成"无凭证 + 旧摘要"，又回到重放。
	audit := e.AuditDir(in.App, in.ID)
	if err := os.MkdirAll(audit, 0o700); err != nil {
		return nil, nil, errEnv("err.env.mkdir", map[string]any{"path": audit}, err)
	}
	start := time.Now().UTC()
	attempt := runRecord{
		Version:   ProfileVersion,
		App:       in.App,
		ID:        in.ID,
		StartedAt: start.Format(time.RFC3339),
	}
	if err := writeRunRecord(filepath.Join(audit, "run.toml"), attempt); err != nil {
		return nil, nil, err
	}
	if err := e.sweepStaleAudits(); err != nil {
		return nil, nil, err
	}
	// 路径是**确定性**的（按 app/id）：verify 要能自己找到这份证据，随机目录名
	// 会让它只能靠扫描猜。同一实例的并发由 run.lock 挡住，不存在两个会话。
	work := e.AuditWorkDir(in.App, in.ID)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, nil, errEnv("err.env.mkdir", map[string]any{"path": work}, err)
	}
	for _, sub := range []string{"upper", "work", "merged"} {
		if err := os.Mkdir(filepath.Join(work, sub), 0o700); err != nil {
			os.RemoveAll(work)
			return nil, nil, errEnv("err.env.mkdir", map[string]any{"path": sub}, err)
		}
	}
	pubs, err := e.probePublicBinds()
	if err != nil {
		os.RemoveAll(work)
		return nil, nil, err
	}
	mounter := e.Mounter
	if mounter == nil {
		mounter = fuseOverlayMounter{}
	}
	unmount, err := mounter.Mount(e.Resolver.Home, filepath.Join(work, "upper"),
		filepath.Join(work, "work"), filepath.Join(work, "merged"))
	if err != nil {
		os.RemoveAll(work)
		return nil, nil, err
	}
	// 拆成两个变量再返回：gofmt 1.26 与 1.27 对「return 多个复合字面量」
	// 的缩进规则不同，写成一条 return 会让 CI（1.26.0）与本地（1.27）
	// 互相判为未格式化。赋值语句的格式在两个版本一致。
	sess := &AuditSession{
		e: e, in: in, p: p, work: work,
		upper:  filepath.Join(work, "upper"),
		start:  start,
		stop:   unmount,
		pubs:   pubs,
		shared: e.existingSharedWrites(p),
		base:   e.Base,
	}
	layers := &AuditLayers{
		Merged:      filepath.Join(work, "merged"),
		PublicBinds: pubs,
		WriteRoot:   writeRoot,
	}
	return sess, layers, nil
}

// existingSharedWrites 是本次审计真正可能生效的 shared_writes：
// BuildBwrapArgs 用 `--bind-try`，不存在的那条会被 bwrap 跳过；
// 把它记进"本该被 bind"的集合会让 verify 把一次正常的写入误判成摘出失效。
func (e *Env) existingSharedWrites(p Profile) []string {
	var out []string
	for _, raw := range p.SharedWrites {
		abs, err := e.Resolver.Expand(raw)
		if err != nil {
			continue
		}
		if _, err := os.Lstat(abs); err == nil {
			out = append(out, abs)
		}
	}
	sort.Strings(out)
	return out
}

// finish 在应用退出后调用：停挂载、写 run.toml、**保留 upper**。
//
// run.toml 是"这次审计正常跑完"的凭证，也是 verify 判断证据完不完整的唯一依据；
// 所以在应用退出之后才写——中途崩掉就不该留下它（那正是"证据不完整"的定义）。
func (s *AuditSession) finish(exitCode int) error {
	if err := s.detach(); err != nil {
		return err
	}
	audit := s.e.AuditDir(s.in.App, s.in.ID)
	if err := os.MkdirAll(audit, 0o700); err != nil {
		return errEnv("err.env.mkdir", map[string]any{"path": audit}, err)
	}
	publicDsts := make([]string, 0, len(s.pubs))
	for _, pb := range s.pubs {
		publicDsts = append(publicDsts, pb.Dst)
	}
	sort.Strings(publicDsts)
	paths := make([]string, 0, len(s.p.Paths))
	for _, raw := range s.p.Paths {
		if abs, err := s.e.Resolver.Expand(raw); err == nil {
			paths = append(paths, abs)
		}
	}
	sort.Strings(paths)
	rec := runRecord{
		Version:       ProfileVersion,
		App:           s.in.App,
		ID:            s.in.ID,
		StartedAt:     s.start.Format(time.RFC3339),
		FinishedAt:    time.Now().UTC().Format(time.RFC3339),
		ExitCode:      exitCode,
		PublicBinds:   publicDsts,
		SharedBinds:   s.shared,
		Paths:         paths,
		Base:          s.base,
		MaskedBase:    Real(s.base),
		BindsRecorded: true,
	}
	return writeRunRecord(filepath.Join(audit, "run.toml"), rec)
}

// writeRunRecord 是 run.toml 的唯一写入口：开始（进行中）与结束（已完成）
// 必须同一套编码，避免两种写法对 BindsRecorded 之类字段的处理出现分叉。
func writeRunRecord(path string, rec runRecord) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(rec); err != nil {
		return errInternal("err.internal.encode_profile", nil, err)
	}
	return writeFileAtomic(path, buf.Bytes(), 0o600)
}

// abandon 收尾一条没走到 finish 的会话。started 表示应用**已经启动过**：
// 那时写层里就可能有用户文件（哪怕 Wait 返回的是非 ExitError），只能卸载后
// 原样保留，让下一次 verify 判 audit_incomplete——删掉等于销毁证据。
func (s *AuditSession) abandon(started bool) {
	_ = s.detach()
	if !started {
		os.RemoveAll(s.work)
	}
}

func (s *AuditSession) detach() error {
	if s.stop == nil {
		return nil
	}
	err := s.stop()
	s.stop = nil
	return err
}

// runRecord 是 `audit/run.toml` 的形状（§6.5）。
//
// 文件从审计**开始**就存在（finished_at 为空 = 进行中），应用退出后由 finish
// 覆写成完整记录。**完成与否只看 FinishedAt**：旧实现"开始前删掉、结束后才写"
// 会漏掉一类假通过——新一轮审计在写层建立前就失败时，目录里只剩旧
// summary.toml，verify 会把它重放成"本次通过"。
//
// PublicBinds / SharedBinds / Paths / Base 记的是**本次审计实际 bind 的清单**：
// verify 依据它判"本该被 bind 走的路径出现在 upper"，而不是依据"配置文件里
// 写了什么"——后者会把没配置或当时不存在的目录也算进去，凭空判 extract_failed。
//
// BindsRecorded 是新旧格式的分界线：老版本（写入器第一版）没有这份清单，
// 读到时必须回退到"按档案推断"，否则真实的 extract_failed 会被判成
// leak-detected（退出 8，下一步是"重新探测"——完全错的方向）。
//
// 版本策略：目前只有 v1。读到 version 大于 ProfileVersion 的记录时，不能按
// v1 的口径解释未知字段——见 Verify 对 rec.Version > ProfileVersion 的处理。
type runRecord struct {
	Version     int      `toml:"version"`
	App         string   `toml:"app"`
	ID          string   `toml:"id"`
	StartedAt   string   `toml:"started_at"`
	FinishedAt  string   `toml:"finished_at,omitempty"`
	ExitCode    int      `toml:"exit_code"`
	PublicBinds []string `toml:"public_binds,omitempty"`
	SharedBinds []string `toml:"shared_binds,omitempty"`
	Paths       []string `toml:"paths,omitempty"`
	Base        string   `toml:"base,omitempty"`
	// MaskedBase 是本次审计用 `--tmpfs` 遮蔽的 AC_BASE 真实路径（v1.0.3 起）。
	// Base 下出现写入说明遮蔽没生效，verify 判 extract_failed 而不是普通 leak。
	MaskedBase    string `toml:"masked_base,omitempty"`
	BindsRecorded bool   `toml:"binds_recorded,omitempty"`
}

// complete 判定"这次审计正常跑完"。finished_at 是 finish 在应用退出之后才
// 写下的，因此它非空才代表写层里的内容是完整会话的证据。
func (r runRecord) complete() bool { return r.FinishedAt != "" }

// readRunRecord 读本次审计的凭证。文件不存在返回 has=false（老实例没跑过审计）；
// 文件存在但解析不了同样返回 has=true（调用方必须按"证据不完整"处理，
// 不能返回环境错误：那样连续重放都会卡在 3，用户没有任何出路）。
func readRunRecord(path string) (runRecord, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return runRecord{}, false, nil
		}
		return runRecord{}, false, errEnv("err.internal.read_profile", map[string]any{"path": path}, err)
	}
	var r runRecord
	if _, derr := toml.Decode(string(data), &r); derr != nil {
		return runRecord{}, true, nil
	}
	return r, true, nil
}

// quarantinePendingAudit 处理上一轮没被 verify 消费的写层。
//
// 旧实现在这里直接拒绝开始新审计（"先 verify 或先取回文件"）。问题是：用户按
// 提示取回文件后没有任何命令能消费那份证据——`run --audit` 永远被拒，唯一出路
// 是手工删文件，违反"复杂度不泄漏给用户"。
//
// 现在改为**搬走而不是拒绝**：写层里有内容就挪到实例 audit/quarantine-<唯一名>
// （保留文件、永不自动删），临时目录随后清掉。新审计与旧证据互不覆盖，
// 用户想找回文件时路径仍在摘要/verify 的输出里。
func (e *Env) quarantinePendingAudit(in Instance) error {
	work := e.AuditWorkDir(in.App, in.ID)
	// 上一轮若是被 SIGKILL 掉的，overlay 可能还挂在 work/merged 上
	// （fuse-overlayfs 1.18 忽略 auto_unmount）。此时搬走 upper 等于让仍活着的
	// fuse 进程继续引用一个已改名目录，必须先把挂载卸掉——与 sweepStaleAudits
	// 的顺序一致，否则"先搬后扫"会在这条崩溃路径上反过来。
	if merged := filepath.Join(work, "merged"); mounted(merged) {
		if err := umount(merged); err != nil {
			return errEnv("err.probe.stale_mount", map[string]any{"path": merged}, err)
		}
	}
	upper := filepath.Join(work, "upper")
	entries, present, err := readUpperTree(upper)
	if err != nil {
		return err
	}
	if present && len(entries) > 0 {
		audit := e.AuditDir(in.App, in.ID)
		if err := os.MkdirAll(audit, 0o700); err != nil {
			return errEnv("err.env.mkdir", map[string]any{"path": audit}, err)
		}
		q := filepath.Join(audit, "quarantine-"+time.Now().UTC().Format("20060102T150405")+"-"+randSuffix())
		if err := moveDir(upper, q); err != nil && !dstComplete(err) {
			// 搬不动就拒绝开始：覆盖现场才是真正的数据丢失。副本完整、只是
			// 源删不干净时按已搬走处理，残留交给下一次清扫。
			return errEnv("err.probe.quarantine_failed", map[string]any{"path": q}, err)
		}
	}
	return os.RemoveAll(work)
}

// sweepStaleAudits 清掉上一轮崩掉留下的 audit 写层（同 probe 的规则：先卸载、
// 有内容就保留而不是删）。**只清扫真正的残留**：
//
// 写层目录名是 `<app>.<id>`，所以清扫前先看对应实例：
//   - run.lock 被持有（run 或 verify 正在用）→ 跳过，绝不动别人的现场；
//   - 实例 audit/run.toml 存在（本轮已正常跑完，等待 verify）→ 跳过；
//     那不是 stale，搬去 /var/tmp 会把证据与实例失联，verify 再也找不到。
//
// 早期版本按 glob 全量清，两个实例同时审计时 B 会卸掉 A 的挂载、把 A 的
// 证据改名——正是"不阻塞新审计"放大出来的多实例事故。
func (e *Env) sweepStaleAudits() error {
	if err := e.sweepAuditRoot(e.tmpRoot()); err != nil {
		return err
	}
	// ADR-033 之前审计写层在 /var/tmp：升级上来的机器里可能留着崩溃残留，
	// 新根扫不到它们。清扫规则完全相同（按实例判活、读失败就保留）。
	if e.TmpRoot == "" && legacyTmpRoot != e.tmpRoot() {
		return e.sweepAuditRoot(legacyTmpRoot)
	}
	return nil
}

func (e *Env) sweepAuditRoot(root string) error {
	dirs, _ := filepath.Glob(filepath.Join(root, "appclone", fmt.Sprintf("audit.%d", e.UID), "*"))
	for _, d := range dirs {
		// 属主判断用进程真实 uid，不用 e.UID（可注入的配置值）：见 sweepProbeRoot。
		if !dirOwnedBy(d, os.Getuid()) {
			continue
		}
		skip, release := e.lockAuditWork(filepath.Base(d))
		if skip {
			release()
			continue
		}
		serr := e.sweepOneAudit(d)
		release()
		if serr != nil {
			return serr
		}
	}
	return nil
}

// sweepOneAudit 处理一个已确认无人持有的写层目录。
//
// 读不出 upper（目录被 chmod 000、子树 IO 错误）时按"可能有内容"处理，搬去
// quarantine 而不是删除：读失败恰恰说明里面有我们看不见的东西，而它们可能
// 是用户文件。跨设备拷贝读不动这样的子树，quarantineDir 会退回写层根内的
// 同 fs rename，现场仍然留下。
func (e *Env) sweepOneAudit(d string) error {
	merged := filepath.Join(d, "merged")
	if mounted(merged) {
		if err := umount(merged); err != nil {
			return errEnv("err.probe.stale_mount", map[string]any{"path": merged}, err)
		}
	}
	entries, present, rerr := readUpperTree(filepath.Join(d, "upper"))
	if rerr == nil && present && len(entries) == 0 {
		os.RemoveAll(d)
		return nil
	}
	if _, err := e.quarantineDir(d); err != nil {
		return errEnv("err.probe.quarantine_failed", map[string]any{"path": d}, err)
	}
	return nil
}

// lockAuditWork 判断某个写层目录是否属于"现在不能碰"的实例；可以碰时
// **持有对应实例的 run.lock 直到 release()**——判断与搬移之间存在窗口，
// 不持锁的话，另一进程可能在中间启动 run，随后被我们卸掉挂载。
//
// 名字解析失败（不是合法的 app.id）按"无人认领"处理：那是真正的残留，
// 宁可清掉也不能让 /tmp 永久堆积挂载点。
//
// 代价：sweepOneAudit 期间本实例的并发 run/verify 会拿到 exit 7（"实例在运行"），
// 即使它实际没在跑。持锁时长等于一次 rename/RemoveAll 的耗时（通常毫秒级），
// 这是"判断→搬移"原子化的最小代价——释放后再搬会重新打开竞态窗口，
// 而那个窗口的后果是卸掉另一个进程正在使用的挂载。
func (e *Env) lockAuditWork(base string) (skip bool, release func()) {
	noop := func() {}
	i := strings.LastIndex(base, ".")
	if i <= 0 {
		return false, noop
	}
	app, id := base[:i], base[i+1:]
	if ValidateAppKey(app) != nil || ValidateID(id) != nil {
		return false, noop
	}
	// 已正常跑完、等 verify 的证据：不是 stale。
	if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "run.toml")); err == nil {
		return true, noop
	}
	// run 与 verify 都持同一把 run.lock；拿不到 = 正在用。
	lockPath := filepath.Join(e.InstancePath(app, id), "run.lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		return false, noop // 实例已删除或从未跑过 → 残留可清
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return true, noop
	}
	return false, func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}
}
