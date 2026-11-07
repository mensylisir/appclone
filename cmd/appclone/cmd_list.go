package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// listJSON 是 list --json 的固定外形（§11）。字段名不许随手改：
// 脚本与 Go 测试都按它取值。
type listJSON struct {
	Schema int       `json:"schema"`
	Apps   []appJSON `json:"apps"`
}

type appJSON struct {
	App        string         `json:"app"`
	Name       string         `json:"name"`
	HasProfile bool           `json:"has_profile"`
	Creatable  bool           `json:"creatable"`
	Instances  []instanceJSON `json:"instances"`
}

type instanceJSON struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Running     bool   `json:"running"`
	LastUsedAt  string `json:"last_used_at,omitempty"`
}

func newListCmd(a *app) *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "list [app]",
		Short: "列出应用与分身",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.applyOverrides(cmd); err != nil {
				return err
			}
			ctx := context.Background()
			apps, scanErrs := a.env.ListApps(ctx)
			filter := ""
			if len(args) == 1 {
				filter = strings.ToLower(args[0])
			}
			// 只读命令不持全局锁（§4.2）：列个表就把 CLI 卡 5 秒没有道理。
			out := listJSON{Schema: 1}
			for _, ap := range apps {
				if filter != "" && ap.Key != filter {
					continue
				}
				// 默认只显示"配过或建过"的应用：把 397 个从没碰过的应用全列出来
				// 不是列表，是倾倒。要全量用 --all（应用集仍是三目录 ∪ 档案 ∪ 实例）。
				if filter == "" && !all && !ap.HasProfile && ap.InstanceN == 0 {
					continue
				}
				insts, err := a.env.ListInstances(ap.Key)
				if err != nil {
					return err
				}
				entry := appJSON{App: ap.Key, Name: ap.Name, HasProfile: ap.HasProfile, Creatable: ap.Creatable}
				for _, in := range insts {
					running, _ := a.env.IsRunning(ap.Key, in.ID)
					ij := instanceJSON{ID: in.ID, DisplayName: in.DisplayName, Running: running}
					if in.LastUsedAt != nil {
						ij.LastUsedAt = in.LastUsedAt.UTC().Format("2006-01-02T15:04:05Z")
					}
					entry.Instances = append(entry.Instances, ij)
				}
				out.Apps = append(out.Apps, entry)
			}
			sort.Slice(out.Apps, func(i, j int) bool { return out.Apps[i].App < out.Apps[j].App })

			a.ok(out, func() {
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "应用\t分身\t状态\t最近使用")
				for _, ap := range out.Apps {
					if len(ap.Instances) == 0 {
						state := "已关闭"
						if !ap.Creatable && ap.HasProfile {
							state = "档案已失效"
						}
						fmt.Fprintf(w, "%s\t-\t%s\t-\n", ap.App, state)
						continue
					}
					for _, in := range ap.Instances {
						state := "已停止"
						if in.Running {
							state = "运行中"
						}
						last := in.LastUsedAt
						if last == "" {
							last = "从未运行"
						}
						fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ap.App, in.DisplayName, state, last)
					}
				}
				w.Flush()
				for _, se := range scanErrs {
					// 用 se.Err 自己的键渲染：这里曾经写死 err.app.invalid_key，
					// 结果是"启动命令解析失败"被显示成"应用标识非法"，排障时把人带偏。
					fmt.Fprintf(os.Stderr, "appclone: 跳过 %s（%s）\n", se.Path, a.describe(se.Err))
				}
			})
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "列出全部应用（默认只列配过或建过分身的）")
	return c
}
