package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mensylisir/appclone/core"
)

func newRmCmd(a *app) *cobra.Command {
	var purgeProfile bool
	c := &cobra.Command{
		Use:   "rm <app> <id>",
		Short: "删除分身（连同桌面入口移入回收站）",
		Long: `删除分身并把数据移入回收站。只删实例与入口，永不删档案——
删了档案，界面上的"配过但关掉"状态就消失，用户得重跑探测。
要同时清掉档案用 --purge-profile。`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.applyOverrides(cmd); err != nil {
				return err
			}
			appKey, id := args[0], args[1]
			ctx := context.Background()
			st, err := a.env.OpenState(ctx, lockTimeout)
			if err != nil {
				return err
			}
			defer st.Close()

			if err := a.env.DeleteInstance(ctx, appKey, id); err != nil {
				return err
			}
			if purgeProfile {
				insts, err := a.env.ListInstances(appKey)
				if err != nil {
					return err
				}
				if len(insts) == 0 {
					if err := a.env.Trasher(a.env.ProfilePath(appKey)); err != nil {
						return err
					}
				}
			}
			a.ok(map[string]any{"app": appKey, "id": id, "trashed": true}, func() {
				fmt.Printf("已把「%s」的分身「%s」移入回收站（可恢复）。\n", appKey, id)
			})
			return nil
		},
	}
	c.Flags().BoolVar(&purgeProfile, "purge-profile", false, "最后一个分身删除后，一并把档案移入回收站")
	return c
}

func newRepairCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "repair-desktop [app]",
		Short: "重建失效的桌面入口（appclone 升级/搬家后 TryExec 指向旧路径）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.applyOverrides(cmd); err != nil {
				return err
			}
			ctx := context.Background()
			st, err := a.env.OpenState(ctx, lockTimeout)
			if err != nil {
				return err
			}
			defer st.Close()

			apps, _ := a.env.ListApps(ctx)
			fixed := 0
			for _, ap := range apps {
				if len(args) == 1 && ap.Key != args[0] {
					continue
				}
				insts, err := a.env.ListInstances(ap.Key)
				if err != nil {
					return err
				}
				for _, in := range insts {
					d, err := a.env.LoadApp(ap.Key)
					if err != nil {
						// 应用已卸载：入口保留但无法重建，交给用户决定删不删实例。
						continue
					}
					if err := a.env.WriteLauncher(in, d); err != nil {
						return err
					}
					fixed++
				}
			}
			a.ok(map[string]any{"repaired": fixed}, func() {
				fmt.Printf("已重建 %d 个桌面入口。\n", fixed)
			})
			return nil
		},
	}
}

var _ = core.CodeOK
