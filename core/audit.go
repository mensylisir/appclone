package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// AuditSession 是一次 `run --audit` 的现场（§6.5）。
//
// 与探测的区别只有两处，其余完全复用运行期那套：多一层 overlay（写层留证据）、
// 结束时不删写层而是写 `audit/run.toml`。**这两处正是"审计能给出结论"的全部前提**：
// 没有写层就没有证据，没有 run.toml 就无法判断"这次审计是不是正常跑完的"。
type AuditSession struct {
	e     *Env
	in    Instance
	work  string // /var/tmp/appclone-audit-<uid>-<rand>
	upper string // work/upper
	start time.Time
	// stop 是卸载 overlay 的闭包；finish/abort 之后置 nil，避免二次卸载。
	stop func() error
}

// beginAudit 准备审计会话并返回要插进 bwrap 参数的那一层。
//
// 三道前置（§6.5）：① 上一轮还有未消费的证据就拒绝开始（先 verify，否则现场被覆盖）；
// ② 扫掉残留（含仍挂载的）；③ 写层必须在 $HOME 之外（overlay 的 upper 嵌在 lower 里会 ELOOP）。
func (e *Env) beginAudit(in Instance) (*AuditSession, *AuditLayers, error) {
	if pending, err := e.auditEvidencePending(in); err != nil {
		return nil, nil, err
	} else if pending != "" {
		return nil, nil, errEnv("err.audit.pending_evidence", map[string]any{"path": pending}, nil)
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
	return &AuditSession{
			e: e, in: in, work: work,
			upper: filepath.Join(work, "upper"),
			start: time.Now().UTC(),
			stop:  unmount,
		}, &AuditLayers{
			Merged:      filepath.Join(work, "merged"),
			PublicBinds: pubs,
		}, nil
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
	rec := runRecord{
		Version:    ProfileVersion,
		App:        s.in.App,
		ID:         s.in.ID,
		StartedAt:  s.start.Format(time.RFC3339),
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
		ExitCode:   exitCode,
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(rec); err != nil {
		return errInternal("err.internal.encode_profile", nil, err)
	}
	return writeFileAtomic(filepath.Join(audit, "run.toml"), buf.Bytes(), 0o600)
}

// abort 用于"应用根本没跑起来"的路径：卸载并删掉写层（里面不可能有用户文件，
// 因为会话没有开始）。有 evidence 时不会走到这里——finish 才保留。
func (s *AuditSession) abort() {
	_ = s.detach()
	os.RemoveAll(s.work)
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
type runRecord struct {
	Version    int    `toml:"version"`
	App        string `toml:"app"`
	ID         string `toml:"id"`
	StartedAt  string `toml:"started_at"`
	FinishedAt string `toml:"finished_at"`
	ExitCode   int    `toml:"exit_code"`
}

// auditEvidencePending 判断上一轮是否还有"没被 verify 消费"的证据。
//
// 有 quarantine-*（越界现场）或摘要里写着 evidence_retained，就必须先 verify/人工处理：
// 直接开始新一轮会把现场覆盖掉——而里面可能是用户文件。
func (e *Env) auditEvidencePending(in Instance) (string, error) {
	audit := e.AuditDir(in.App, in.ID)
	qs, _ := filepath.Glob(filepath.Join(audit, "quarantine-*"))
	if len(qs) > 0 {
		return qs[0], nil
	}
	sum, has, err := readSummary(filepath.Join(audit, "summary.toml"))
	if err != nil {
		return "", err
	}
	if has && sum.Retained {
		return filepath.Join(audit, "summary.toml"), nil
	}
	return "", nil
}

// sweepStaleAudits 清掉上一轮崩掉留下的 audit 写层（同 probe 的规则：先卸载、
// 有内容就保留而不是删）。
func (e *Env) sweepStaleAudits() error {
	dirs, _ := filepath.Glob(filepath.Join("/var/tmp", "appclone", fmt.Sprintf("audit.%d", e.UID), "*"))
	for _, d := range dirs {
		merged := filepath.Join(d, "merged")
		if mounted(merged) {
			if err := umount(merged); err != nil {
				return errEnv("err.probe.stale_mount", map[string]any{"path": merged}, err)
			}
		}
		entries, present, rerr := readUpperTree(filepath.Join(d, "upper"))
		if rerr != nil || !present || len(entries) == 0 {
			os.RemoveAll(d)
			continue
		}
		q := fmt.Sprintf("/var/tmp/appclone-quarantine-%d-%s", e.UID,
			time.Now().UTC().Format("20060102T150405")+"-"+randSuffix())
		if err := os.Rename(d, q); err != nil {
			return errEnv("err.probe.quarantine_failed", map[string]any{"path": d}, err)
		}
	}
	return nil
}
