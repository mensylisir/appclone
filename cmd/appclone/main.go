// Command appclone 是 core 的开发者视图（普通用户的主入口是 appclone-gui）。
// 两者共享同一份 core，命令语义与 GUI 操作一一对应（§6 表格）。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/mensylisir/appclone/core"
	"github.com/mensylisir/appclone/core/i18n"
)

// version 由构建期 -ldflags -X main.version=… 注入。
var version = "dev"

type app struct {
	env  *core.Env
	lang string
	json bool
}

func main() {
	a := &app{}
	root := &cobra.Command{
		Use:           "appclone",
		Short:         "为任意 Linux 应用再开一个分身（只隔离它自己的数据）",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			env, err := core.NewEnv()
			if err != nil {
				return err
			}
			a.env = env
			a.lang = i18n.Lang()
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	root.PersistentFlags().BoolVar(&a.json, "json", false, "机器可读输出（成功与失败同一外形）")
	root.PersistentFlags().String("base", "", "覆盖数据目录（等价 AC_BASE，测试用）")
	root.PersistentFlags().String("lang", "", "覆盖界面语言（zh_CN / en）")

	root.AddCommand(
		newAddCmd(a),
		newRunCmd(a),
		newStopCmd(a),
		newListCmd(a),
		newRmCmd(a),
		newDoctorCmd(a),
		newRepairCmd(a),
		newVersionCmd(),
	)
	if err := root.Execute(); err != nil {
		a.fail(err)
	}
}

// fail 是唯一的错误出口：文案走 i18n 键，退出码走 core.ExitCode。
// 脚本按 code/key 分支，不许解析人类文案（§11）。
func (a *app) fail(err error) {
	code := core.ExitCode(err)
	if a.json {
		payload := map[string]any{"ok": false}
		e := &core.Error{Code: code, Key: "err.internal.unknown"}
		if errors.As(err, &e) {
			payload["error"] = map[string]any{
				"code":    e.Code,
				"key":     e.Key,
				"message": i18n.T(a.langOr(), e.Key, e.Args),
				"args":    e.Args,
			}
		} else {
			payload["error"] = map[string]any{
				"code": code, "key": "err.internal.unknown", "message": err.Error(),
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
	} else {
		var e *core.Error
		if errors.As(err, &e) {
			fmt.Fprintln(os.Stderr, "appclone: "+i18n.T(a.langOr(), e.Key, e.Args))
			if e.Err != nil && os.Getenv("APPCLONE_DEBUG") != "" {
				fmt.Fprintln(os.Stderr, "  原因:", e.Err)
			}
		} else {
			fmt.Fprintln(os.Stderr, "appclone: "+err.Error())
		}
	}
	os.Exit(code)
}

func (a *app) langOr() string {
	if a.lang == "" {
		return "en"
	}
	return a.lang
}

// ok 输出成功结果：人类可读走 human，--json 走同一份 data。
func (a *app) ok(data any, human func()) {
	if a.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"ok": true, "data": data})
		return
	}
	if human != nil {
		human()
	}
}

// describe 把任意 error 渲染成一句人话：*core.Error 走 i18n，其余原样。
func (a *app) describe(err error) string {
	var e *core.Error
	if errors.As(err, &e) {
		return i18n.T(a.langOr(), e.Key, e.Args)
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func (a *app) tr(key string, args map[string]any) string {
	return i18n.T(a.langOr(), key, args)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "打印版本",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(version)
			return nil
		},
	}
}

// newEnvAfterFlags 让 --base / --lang 生效。cobra 的 PersistentPreRunE 里
// 已经构造过 Env，这里只处理覆盖：路径解析只有一份实现，覆盖也必须走同一条。
func (a *app) applyOverrides(cmd *cobra.Command) error {
	if base, _ := cmd.Flags().GetString("base"); base != "" {
		os.Setenv("AC_BASE", base)
		env, err := core.NewEnv()
		if err != nil {
			return err
		}
		a.env = env
	}
	if lang, _ := cmd.Flags().GetString("lang"); lang != "" {
		a.lang = lang
	}
	return nil
}

// timeout 是全局写锁的等待上限（§4.2）。5 秒是"人能等，脚本能判"的折中。
const lockTimeout = 5 * time.Second
