package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mensylisir/appclone/core"
)

func newStopCmd(a *app) *cobra.Command {
	c := &cobra.Command{
		Use:   "stop <app> <id>",
		Short: "停止正在运行的分身",
		Long: `停止正在运行的分身。

先确认 run.lock 被持有，再向 run 进程发 SIGTERM；run 会对应用的整个进程组
SIGTERM，3 秒后 SIGKILL。没有在运行、或无法确认运行它的进程时退出 7——
绝不盲杀 PID。`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.applyOverrides(cmd); err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Ctrl-C 只取消等待：SIGTERM 已经发给 run，应用的终止链继续跑。
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
			defer signal.Stop(sig)
			go func() {
				<-sig
				cancel()
			}()

			if err := a.env.StopRun(ctx, args[0], args[1]); err != nil {
				// 用户取消不是失败（§11 sentinel）。
				if errors.Is(err, core.ErrCanceled) {
					return nil
				}
				return err
			}
			a.ok(map[string]any{"stopped": true}, func() {
				fmt.Fprintln(cmd.OutOrStdout(), a.tr("ui.stop.stopped", map[string]any{
					"app": args[0], "id": args[1],
				}))
			})
			return nil
		},
	}
	return c
}
