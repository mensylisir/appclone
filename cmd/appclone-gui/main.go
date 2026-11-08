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
		// errorArgs 不能省：err.env.ac_home_relative 这类模板带 {path}，
		// 不传参数会把字面量 {path} 打到 stderr 上。
		lang := i18n.Lang()
		fmt.Fprintln(os.Stderr, "appclone-gui:", i18n.T(lang, errorKey(err), errorArgs(err)))
		os.Exit(int(core.ExitCode(err)))
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone", gio.GApplicationFlagsNoneValue)
	defer app.Unref()

	// ADR-027 退场：三屏四态的静态文案已全部按 ui.* 进消息表（zh_CN + en），
	// GTK 闸门走遍列表/配置/详情三屏与 probing/confirm/creating/error 四态，
	// 所以界面语言可以跟随 LANG。动态文案（候选路径的 reason、core 错误）
	// 与静态文案共用同一张表，不会再出现"中文界面插一句英文"。
	u := newUI(app, env)
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
