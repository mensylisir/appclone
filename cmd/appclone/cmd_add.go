package main

import (
	"context"
	"fmt"
	"strings"

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
			appKey := strings.ToLower(args[0])
			if err := core.ValidateAppKey(appKey); err != nil {
				return err
			}
			id := ""
			if len(args) == 2 {
				id = args[1]
			}
			ctx := context.Background()
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
				if !ok {
					// 探测管线尚未接入（discover.md 的三层管线在下一增量实现）。
					// 这里必须明确报"没有可用档案"，而不是悄悄造一个空档案。
					return core.NewProbeUnavailable(appKey)
				}
				p = bundled
				if err := a.env.SaveProfile(p); err != nil {
					return err
				}
			} else {
				if p, err = a.env.LoadProfile(appKey); err != nil {
					return err
				}
			}

			if profileOnly {
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
