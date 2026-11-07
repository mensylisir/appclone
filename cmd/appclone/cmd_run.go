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

func newRunCmd(a *app) *cobra.Command {
	var (
		audit         bool
		wait          bool
		propagateExit bool
	)
	c := &cobra.Command{
		Use:   "run <app> <id> [-- args...]",
		Short: "在分身环境中启动应用",
		Long: `在分身环境中启动应用。

语法边界写死：-- 之前是 appclone 的 flag，-- 之后原样透传给应用
（%U 展开的 URI、以 - 开头的应用参数都靠它）。不带 -- 时，
第一个非 flag 参数之后的全部算应用参数。`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.applyOverrides(cmd); err != nil {
				return err
			}
			appKey, id := args[0], args[1]
			var pass []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				pass = args[dash:]
			} else if len(args) > 2 {
				pass = args[2:]
			}

			if audit {
				// 审计层还没实现：明确拒绝，不做静默 no-op（§6.5）。
				return core.NewAuditUnavailable()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Ctrl-C 交给 Run 内部的进程组处理：它要先把应用带走，再自己退。
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			go func() {
				<-sig
				cancel()
			}()

			res, err := a.env.Run(ctx, appKey, id, core.RunOptions{
				Args:          pass,
				Audit:         audit,
				Wait:          wait,
				PropagateExit: propagateExit,
				Stdout:        os.Stdout,
				Stderr:        os.Stderr,
			})
			if err != nil {
				// 用户取消不是失败：0 退出（§11 sentinel）。
				if errors.Is(err, core.ErrCanceled) {
					return nil
				}
				return err
			}
			code := 0
			switch {
			case res.Signaled:
				code = 128 + int(res.Signal)
			case res.AppExitCode != 0:
				// 不猜应用码的含义：把 2~8 原样透出去会把 appclone 自己的
				// 错误码体系撕开（§6.3）。脚本要原值就加 --propagate-exit。
				if propagateExit {
					code = res.AppExitCode
				} else {
					code = 1
				}
			}
			if a.json {
				a.ok(map[string]any{
					"app_exit_code": res.AppExitCode,
					"signal":        signalName(res),
					"signaled":      res.Signaled,
				}, nil)
			}
			os.Exit(code)
			return nil
		},
	}
	// 关掉 flag 交错解析：`run app main --user-data-dir=x` 必须把 --user-data-dir
	// 当成应用参数，而不是 appclone 的未知 flag（§6 语法边界）。
	c.Flags().SetInterspersed(false)
	c.Flags().BoolVar(&audit, "audit", false, "开启运行期审计层（供 doctor --verify 对账；本版本未实现）")
	c.Flags().BoolVar(&wait, "wait", false, "该分身已在运行时等待它退出（默认立刻返回）")
	c.Flags().BoolVar(&propagateExit, "propagate-exit", false, "原样返回应用的退出码")
	return c
}

func signalName(res core.RunResult) string {
	if !res.Signaled {
		return ""
	}
	if name, ok := signalNames[res.Signal]; ok {
		return name
	}
	return fmt.Sprintf("SIG%d", int(res.Signal))
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGTERM: "SIGTERM",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGHUP:  "SIGHUP",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGABRT: "SIGABRT",
}
