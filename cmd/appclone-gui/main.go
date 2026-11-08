// appclone-gui 是普通用户的主入口（§6）：三屏都用 libadwaita 原生控件，
// 不写自定义 CSS，不要求用户开终端。
package main

import (
	"fmt"
	"os"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"

	"github.com/mensylisir/appclone/core"
	"github.com/mensylisir/appclone/core/i18n"
)

func main() {
	env, err := core.NewEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "appclone-gui:", i18n.T(i18n.Lang(), errorKey(err), nil))
		os.Exit(int(core.ExitCode(err)))
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone", gio.GApplicationFlagsNoneValue)
	defer app.Unref()

	// GUI 的静态文案目前只有中文（i18n 目录里没有 ui.* 键，本地化见 ADR-025），
	// 所以动态文案（候选路径的 reason、core 错误）也必须用同一语言：直接跟
	// LANG 走会出现"中文界面插进一句英文"。语言标签必须是目录里真实的键
	// （"zh_CN"，不是 "zh"——i18n.T 查不到键时会静默回退到英文）。
	// 等 GUI 文案进目录再改回 i18n.Lang()。
	u := &ui{app: app, env: env, lang: "zh_CN"}
	// 第二个实例是合法的：GApplication 会把它转发给已运行的那一个。
	// 此时只把已有窗口提到前台，绝不能再建一个窗口（两份列表/探测状态机
	// 会各自写同一份档案目录）。
	actcb := func(_ gio.Application) {
		if u.win != nil {
			u.win.Present()
			return
		}
		u.buildWindow()
	}
	app.ConnectActivate(&actcb)
	u.hold(&actcb)

	if code := app.Run(int32(len(os.Args)), os.Args); code > 0 {
		os.Exit(int(code))
	}
}
