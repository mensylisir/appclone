package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mensylisir/appclone/core"
)

func newAddCmd(a *app) *cobra.Command {
	var (
		rediscover  bool
		profileOnly bool
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
  add <app> --profile-only         只保存档案`,
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
			// 探测会把应用跑起来，用户随时可能 Ctrl-C：信号要能传到 context，
			// 否则 defer 全部跳过、/var/tmp 下留下挂载点与写层（§3.2.3）。
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

			var p core.Profile
			needProfile := !hasProfile || !creatable || rediscover
			if needProfile {
				if !rediscover && hasProfile && !creatable {
					// 档案失效：不重探就只能造出一个启动即失败的分身（§6.1 ②）。
					fmt.Fprintln(cmd.ErrOrStderr(), a.tr("ui.add.reconfigure_note", map[string]any{"app": appKey}))
				}
				bundled, ok, berr := a.env.BundledProfile(appKey)
				if berr != nil {
					return berr
				}
				if ok {
					p = bundled
				} else {
					// 没有内置模板就走探测（discover.md 的三层管线，本版本只有 L1）。
					// 探测会**真的把应用启动一次**：用户用完关掉，会话结束才开始归因。
					d, lerr := a.env.LoadApp(appKey)
					if lerr != nil {
						return lerr
					}
					cmdline := append([]string{d.Exec}, d.ExecArgs...)
					fmt.Fprintf(cmd.ErrOrStderr(),
						"正在跟踪「%s」的写入：应用会被启动一次，请正常用一下再把它关掉。\n", appKey)
					res, perr := a.env.Probe(ctx, appKey, core.ProbeOptions{
						Command: cmdline,
						Stderr:  cmd.ErrOrStderr(),
					})
					if perr != nil {
						return perr
					}
					p = res.Profile
					// 零写入 = 给不出结论，**不生成空档案**：空 paths 的档案会让
					// 开关亮着却什么都不隔离（§6.3）。
					if len(p.Paths) == 0 {
						// 只观测到缓存/共享写入时，"什么都没跟踪到"是错的说法：
						// 用户会去查应用是不是没启动，而真正的原因是这些写入
						// 本来就不值得隔离（§6.3）。
						return core.NewProbeNoWrites(appKey, len(res.Candidates))
					}
				}
			} else {
				if p, err = a.env.LoadProfile(appKey); err != nil {
					return err
				}
			}

			if profileOnly {
				// 只落档案这条路径不经过 CreateInstance 的事务，校验必须自己补一遍：
				// 存档前一个字节都不能写（§11 CreateInstance 事务第 1 步的原则）。
				if err := core.ValidateProfile(p, a.env.BuildOptions()); err != nil {
					return err
				}
				if err := core.ValidateCreatable(p); err != nil {
					return err
				}
				if err := a.env.SaveProfile(p); err != nil {
					return err
				}
				a.ok(map[string]any{
					"app": appKey, "profile_saved": true, "probe_method": p.ProbeMethod,
				}, func() {
					fmt.Printf("已保存「%s」的隔离档案（%d 条路径，来源 %s），未创建分身。\n",
						appKey, len(p.Paths), p.ProbeMethod)
				})
				return nil
			}

			in, err := a.env.CreateInstance(ctx, appKey, p, id)
			if err != nil {
				return err
			}
			a.ok(map[string]any{
				"app": in.App, "id": in.ID, "display_name": in.DisplayName,
				"created_at": in.CreatedAt, "probe_method": in.ProbeMethod,
			}, func() {
				fmt.Printf("已创建「%s（分身 %s）」\n", appKey, in.DisplayName)
				fmt.Printf("  桌面入口：%s\n", a.env.LauncherPath(in.App, in.ID))
				fmt.Printf("  数据目录：%s\n", in.Root)
			})
			return nil
		},
	}
	c.Flags().BoolVar(&rediscover, "rediscover", false, "强制重新探测（会建实例）")
	c.Flags().BoolVar(&profileOnly, "profile-only", false, "只更新档案，不创建实例")
	return c
}
