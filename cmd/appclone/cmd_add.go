package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mensylisir/appclone/core"
)

// isProfileNotFound 区分"没有档案"与"档案读不出来"：前者是首次配置的正常
// 起点，后者（版本超前/损坏）必须原样报错，绝不能当没档案继续写。
func isProfileNotFound(err error) bool {
	var ce *core.Error
	return errors.As(err, &ce) && ce.Key == "err.profile.not_found"
}

func newAddCmd(a *app) *cobra.Command {
	var (
		rediscover  bool
		profileOnly bool
		fork        bool
		noReadTrace bool
	)
	c := &cobra.Command{
		Use:   "add <app> [id]",
		Short: "为应用创建分身（有档案就复用，没有才探测）",
		Long: `默认语义：有合法档案就复用、跳过探测；没有才探测。

  add <app>                        首次配置 / 重新配置 / 开启分身（看档案状态）
  add <app> <id>                   新建一个分身（id 已被占用则报错）
  add <app> --rediscover           重跑探测并建实例
  add <app> --rediscover --profile-only
                                   只重跑探测并保存档案，不建实例（= GUI「重新探测」）
  add <app> --profile-only         只保存档案
  add <app> --fork                 新分身从主实例现有数据克隆起步（写时复制，
                                   不收主实例数据副本；run 时挂 overlay，无 root）`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.applyOverrides(cmd); err != nil {
				return err
			}
			// 用户输入先归一再校验：`add "CC Switch"` 与 `add cc-switch` 是同一个应用，
			// 而 "CC Switch" 的 .desktop 文件名里确实带空格（§4 <app> 规则）。
			appKey, err := core.SanitizeAppKey(args[0])
			if err != nil {
				return err
			}
			// add 与 run 都要过免 root 红线（§3.1 末段）。
			if err := core.CheckNotRoot(); err != nil {
				return err
			}
			id := ""
			if len(args) == 2 {
				id = args[1]
			}
			if profileOnly && id != "" {
				// 只保存档案却给了实例 id：静默忽略会让人以为实例建了。
				return core.NewUsage("--profile-only 不创建实例，不要再给 <id>")
			}
			if profileOnly && fork {
				// --fork 描述的是实例的数据层来源；没有实例时它无处落地。
				return core.NewUsage("--fork 需要创建实例，不能与 --profile-only 同用")
			}
			// 探测会把应用跑起来，用户随时可能 Ctrl-C：信号要能传到 context，
			// 否则 defer 全部跳过、写层根下留下挂载点与写层（§3.2.3）。
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// 写操作全程持全局锁：命名空间变更必须串行化（§4.2）。
			st, err := a.env.OpenState(ctx, lockTimeout)
			if err != nil {
				return err
			}
			defer st.Close()

			hasProfile, creatable, err := a.env.ProfileState(appKey)
			if err != nil {
				return err
			}
			if !hasProfile {
				// 没有档案时先确认应用真的存在：不存在是输入错误（2），
				// 不该报成"探测给不出结论"（4）——脚本对这两类的处理完全不同（§6.3）。
				if _, lerr := a.env.LoadApp(appKey); lerr != nil {
					return lerr
				}
			}

			// 既有档案只读一次：它既是"可复用"的来源，也是重新配置时的非路径
			// 字段基线。读不出来（版本超前 / 损坏）**不能**当"没档案"继续探测，
			// 否则后面会把它覆盖掉——§5.5 要求拒绝加载并提示。
			var old core.Profile
			hasOld := false
			if hasProfile {
				op, oerr := a.env.LoadProfile(appKey)
				if oerr != nil {
					if !isProfileNotFound(oerr) {
						return oerr
					}
				} else {
					old, hasOld = op, true
				}
			}

			var p core.Profile
			// 本次探测的读证据告警只对这一次运行有意义（run-scoped），不写进
			// 档案；`--json` 直接展示，GUI 用同一个字段。
			var readsWarns []string
			// 探测证据（run-scoped，不落盘）：keyring_evidence 只在本次真的
			// 探测过时才有意义；复用档案时是空串（"没有证据"而不是"没有 keyring"）。
			var keyringEvidence string
			// 证据扫描被上限截断时非零：没有观测到 keyring 不等于应用不用，
			// 必须让脚本能区分"证据完整但没有"与"证据被截断"。
			var keyringDropped int
			// 自动预填的 exec_args 只来自 RetKnown=false 证据时置位（run-scoped
			// 诊断，只进 --json）。必须声明在 needProfile 之外：两处 payload
			// 都要用。
			var selfRelaunchUnknown, selfRelaunchAmbiguous bool
			var roundMarkerDropped int
			needProfile := !hasProfile || !creatable || rediscover
			if needProfile {
				if !rediscover && hasProfile && !creatable {
					// 档案失效：不重探就只能造出一个启动即失败的分身（§6.1 ②）。
					fmt.Fprintln(cmd.ErrOrStderr(), a.tr("ui.add.reconfigure_note", map[string]any{"app": appKey}))
				}
				// 内置模板只作**预填**，不定案（§4、§6）：「有模板就不探测」会让
				// CLI 与 GUI 对同一应用写出两份语义（manual vs overlay），也让
				// `--rediscover` 名不副实——应用升级后路径变了，模板追不上。
				bundled, hasBundled, berr := a.env.BundledProfile(appKey)
				if berr != nil {
					return berr
				}
				// 探测会**真的把应用启动一次**：用户用完关掉，会话结束才开始归因。
				d, lerr := a.env.LoadApp(appKey)
				if lerr != nil {
					return lerr
				}
				cmdline := core.ProbeCommand(d.Exec, d.ExecArgs, old, hasOld)
				fmt.Fprintf(cmd.ErrOrStderr(),
					"正在跟踪「%s」的写入：应用会被启动两次，每次用一下再关掉。\n", appKey)
				// L0 先扫一遍：它的锁文件线索能让探测不再"零写入"——主实例
				// 在跑时应用会发现锁被占用、把参数转发过去然后自己退出（§3.2.2）。
				hints := a.env.StaticHints(ctx, d)
				res, fin, _, _, perr := a.env.ProbeWithRetry(ctx, appKey, core.ProbeOptions{
					Command:          cmdline,
					LockFile:         hints.LockFiles,
					StaticPaths:      hints.Paths,
					Stderr:           cmd.ErrOrStderr(),
					DisableReadTrace: noReadTrace,
					OnReadsWarn: func(reason string) {
						// 读证据一旦不完整，用户必须在探测过程中就知道，
						// 而不是等确认页/档案落盘后才发现（§11.1）。
						fmt.Fprintf(cmd.ErrOrStderr(), "appclone: %s\n", a.tr("probe.reads."+reason, nil))
					},
					OnRetry: func(args []string) {
						// 零写入 + 自重启证据：解释为什么应用会被多启动一轮，
						// 否则用户只看到"又开始了"。
						fmt.Fprintf(cmd.ErrOrStderr(), "appclone: %s\n",
							a.tr("probe.retry.self_relaunch", map[string]any{"args": strings.Join(args, " ")}))
					},
				}, core.ProbeFinalize{
					Bundled:     bundled,
					HasBundled:  hasBundled,
					Existing:    old,
					HasExisting: hasOld,
				})
				if perr != nil {
					// 用户取消不是失败：有保留现场时让 main 打出路径（错误码
					// 已是 CodeOK），否则静默返回 0（§11 sentinel）。
					if errors.Is(perr, core.ErrCanceled) {
						var ce *core.Error
						if errors.As(perr, &ce) && ce.Key == "err.probe.canceled_kept" {
							return perr
						}
						return nil
					}
					return perr
				}
				// 预填、来源标注、零写入门槛、非路径字段合并只有 core 一份实现
				// （ProbeWithRetry → FinalizeProbe）：GUI 确认页走同一个函数，
				// 否则同一个应用会被两端落出两份档案（§6.1）。
				p = fin
				readsWarns = res.ReadsWarns
				keyringEvidence = res.KeyringEvidence
				keyringDropped = res.KeyringTraceDropped
				selfRelaunchUnknown = res.SelfRelaunchUnknown
				selfRelaunchAmbiguous = res.SelfRelaunchAmbiguous
				roundMarkerDropped = res.RoundMarkerDropped
			} else if hasOld {
				p = old
			} else if p, err = a.env.LoadProfile(appKey); err != nil {
				return err
			}

			if profileOnly {
				// 只落档案这条路径不经过 CreateInstance 的事务，校验必须自己补一遍：
				// 存档前一个字节都不能写（§11 CreateInstance 事务第 1 步的原则）。
				if len(p.Paths) == 0 {
					// CLI 永不进入"降级空确认"分支（AllowEmpty 只给 GUI 首次配置）；
					// 空 paths 的档案等于没隔离，保存它只会制造一份假承诺。
					return core.NewProbeNoWrites(appKey, 0)
				}
				if err := core.ValidateProfile(p, a.env.BuildOptions()); err != nil {
					return err
				}
				if err := core.ValidateCreatable(p); err != nil {
					return err
				}
				if err := a.env.SaveProfile(p); err != nil {
					return err
				}
				payload := map[string]any{
					"app": appKey, "profile_saved": true, "probe_method": p.ProbeMethod,
					"read_trace": p.ReadTrace, "credential_paths": p.CredentialPaths,
					"isolate_keyring": p.IsolateKeyring, "keyring_evidence": keyringEvidence,
					"keyring_trace_dropped": keyringDropped,
				}
				addProbeEvidence(payload, readsWarns, p, selfRelaunchUnknown, selfRelaunchAmbiguous, roundMarkerDropped)
				a.ok(payload, func() {
					fmt.Printf("已保存「%s」的隔离档案（%d 条路径，来源 %s），未创建分身。\n",
						appKey, len(p.Paths), p.ProbeMethod)
					a.printReadEvidence(p, readsWarns)
				})
				return nil
			}

			in, err := a.env.CreateInstanceWith(ctx, appKey, p, id, core.CreateOptions{Fork: fork})
			if err != nil {
				return err
			}
			payload := map[string]any{
				"app": in.App, "id": in.ID, "display_name": in.DisplayName,
				"created_at": in.CreatedAt, "probe_method": in.ProbeMethod, "fork": in.Fork,
				"read_trace": p.ReadTrace, "credential_paths": p.CredentialPaths,
				"isolate_keyring": p.IsolateKeyring, "keyring_evidence": keyringEvidence,
				"keyring_trace_dropped": keyringDropped,
			}
			addProbeEvidence(payload, readsWarns, p, selfRelaunchUnknown, selfRelaunchAmbiguous, roundMarkerDropped)
			a.ok(payload, func() {
				fmt.Printf("已创建「%s（分身 %s）」\n", appKey, in.DisplayName)
				fmt.Printf("  桌面入口：%s\n", a.env.LauncherPath(in.App, in.ID))
				fmt.Printf("  数据目录：%s\n", in.Root)
				if in.Fork {
					fmt.Println("  首次启动会继承主实例现有数据（需要 fuse-overlayfs）；之后的修改只写入这个分身。")
				}
				a.printReadEvidence(p, readsWarns)
			})
			return nil
		},
	}
	c.Flags().BoolVar(&rediscover, "rediscover", false, "强制重新探测（会建实例）")
	c.Flags().BoolVar(&profileOnly, "profile-only", false, "只更新档案，不创建实例")
	c.Flags().BoolVar(&fork, "fork", false, "新分身从主实例现有数据克隆起步（写时复制）")
	// 隐藏开关只给基准/排障用：关闭读追踪后档案的 read_trace 会是
	// unavailable，产品路径不应该让用户自己关掉登录态发现能力。
	c.Flags().BoolVar(&noReadTrace, "no-read-trace", false, "关闭读追踪（排障/基准用）")
	_ = c.Flags().MarkHidden("no-read-trace")
	return c
}

// addProbeEvidence 把 run-scoped 的读证据字段加进 `--json` 输出。空值不写，
// 避免脚本把"没有告警"误读成"告警字段为空数组"。
func addProbeEvidence(payload map[string]any, readsWarns []string, p core.Profile, selfRelaunchUnknown, selfRelaunchAmbiguous bool, roundMarkerDropped int) {
	if len(readsWarns) > 0 {
		payload["reads_warns"] = readsWarns
	}
	if len(p.CredentialDropped) > 0 {
		// 字段名沿用契约：这里携带的是被收缩/丢弃的**旧凭据路径**，不是告警键。
		payload["credential_scope_narrowed"] = p.CredentialDropped
	}
	if len(p.ExecArgs) > 0 {
		// 空值不写键，保持 --json 的 omit-empty 约定；ADR-036 已注明。
		payload["exec_args"] = append([]string{}, p.ExecArgs...)
	}
	if selfRelaunchUnknown {
		payload["self_relaunch_unknown"] = true
	}
	if selfRelaunchAmbiguous {
		payload["self_relaunch_ambiguous"] = true
	}
	if roundMarkerDropped > 0 {
		payload["round_marker_dropped"] = roundMarkerDropped
	}
}

// printReadEvidence 是人类输出里的读证据摘要：三态 + 告警。
func (a *app) printReadEvidence(p core.Profile, readsWarns []string) {
	switch p.ReadTrace {
	case core.ReadTraceUnavailable:
		fmt.Println("  读证据：不可用（本次只依据写入证据）")
	case core.ReadTracePartial:
		fmt.Println("  读证据：不完整（登录态可能没有被完全发现）")
	case core.ReadTraceOK:
		fmt.Println("  读证据：完整")
	}
	for _, w := range readsWarns {
		fmt.Printf("  - %s\n", a.tr("probe.reads."+w, nil))
	}
	if len(p.CredentialPaths) > 0 {
		fmt.Printf("  登录态目录：%s\n", strings.Join(p.CredentialPaths, "、"))
	}
	for _, w := range p.Warnings {
		fmt.Printf("  档案告警：%s\n", a.tr("probe.profile."+w, nil))
	}
	if len(p.CredentialDropped) > 0 {
		fmt.Printf("  已丢弃的旧登录态路径：%s\n", strings.Join(p.CredentialDropped, "、"))
	}
}
