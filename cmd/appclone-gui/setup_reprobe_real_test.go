//go:build linux && gtk_e2e

package main

// 真实链路的旧档案重探冒烟（手动，不进 CI 闸门）。
//
// 与 setup_reprobe_test.go 的分工：那几条用状态机驱动，验证接线与 epoch/
// 恢复语义；这条真的跑 core.Probe：在假 HOME 里手写一份 read_trace="" 的旧
// 档案，点横幅上的「重新探测」，由探测会话启动一个假应用（写两个目录、读回
// 一个文件），落案后确认并建实例，再核对磁盘上的档案与宿主目录。
//
// 需要显示、宿主 bwrap 与可取用的 FUSE 写层（缺一不可，失败时错误页会说明）。
// 显式 opt-in：APPCLONE_GTK_REAL_PROBE=1；guitest-gtk.sh 把它排在 -skip 外
// 的手动清单里，不参与常规闸门的时长与稳定性。
//
// 运行：
//   APPCLONE_GTK_ROOT=$(mktemp -d) APPCLONE_GTK_TEST=1 APPCLONE_GTK_REAL_PROBE=1 \
//     CGO_ENABLED=0 go test -tags gtk_e2e -count=1 -timeout 240s \
//     -run '^TestGtkLegacyReprobeRealProbe$' ./cmd/appclone-gui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"

	"github.com/mensylisir/appclone/core"
)

func TestGtkLegacyReprobeRealProbe(t *testing.T) {
	if os.Getenv("APPCLONE_GTK_REAL_PROBE") != "1" {
		t.Skip("真实探测冒烟需要显式 APPCLONE_GTK_REAL_PROBE=1（需要显示 + bwrap + FUSE）")
	}
	env, appsDir := gtkTestEnv(t, "legacy-real")
	// 用真实 mounter（gtkTestEnv 默认给的是"永远不可用"的 stub）。
	env.Mounter = nil

	home := env.Resolver.Home
	appScript := filepath.Join(env.Base, "fake-demo.sh")
	script := "#!/bin/sh\n" +
		"mkdir -p \"$HOME/.config/Demo\" \"$HOME/.local/share/Demo\"\n" +
		"printf 'x' >> \"$HOME/.config/Demo/state\"\n" +
		"printf 'y' >> \"$HOME/.local/share/Demo/data\"\n" +
		"cat \"$HOME/.config/Demo/state\" >/dev/null 2>&1 || true\n"
	if err := os.MkdirAll(env.Base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appScript, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec="+appScript+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 旧档案：只记了配置目录、没有读证据（旧版本产物）。
	old := core.Profile{
		App: legacyReprobeApp, Version: core.ProfileVersion, Exec: appScript,
		ProbeMethod: "overlay", ReadTrace: "",
		Paths: []string{"~/.config/Demo"}, Env: map[string]string{}, Ignored: core.DefaultIgnored(),
	}
	if err := env.SaveProfile(old); err != nil {
		t.Fatal(err)
	}
	// 探测前置要求能读出 XDG 公共目录清单；假 HOME 里没有会被闸门挡住。
	dirs := "XDG_DESKTOP_DIR=\"$HOME/Desktop\"\n" +
		"XDG_DOWNLOAD_DIR=\"$HOME/Downloads\"\n" +
		"XDG_DOCUMENTS_DIR=\"$HOME/Documents\"\n" +
		"XDG_MUSIC_DIR=\"$HOME/Music\"\n" +
		"XDG_PICTURES_DIR=\"$HOME/Pictures\"\n" +
		"XDG_VIDEOS_DIR=\"$HOME/Videos\"\n" +
		"XDG_PUBLICSHARE_DIR=\"$HOME/Public\"\n" +
		"XDG_TEMPLATES_DIR=\"$HOME/Templates\"\n"
	cfgHome := env.Resolver.XDG["CONFIG_HOME"]
	if err := os.MkdirAll(cfgHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgHome, "user-dirs.dirs"), []byte(dirs), 0o600); err != nil {
		t.Fatal(err)
	}

	// 建实例要生成指向 CLI 的桌面入口；直接跑 go test 时 PATH 里没有
	// appclone，用既有测试注入口钉一个存在且可执行的路径。
	env.CliPath = "/bin/true"

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkLegacyRealProbe",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()
	u := &ui{app: app, env: env, lang: "zh_CN"}

	var runErr error
	stage := 0
	var reloaded core.Profile
	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		u.buildWindow()
		deadline := time.Now().Add(120 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			state := "<nil>"
			desc := ""
			if v := u.activeSetup; v != nil {
				state = v.state
				desc = v.errPage.GetTitle() + " / " + v.errPage.GetDescription()
			}
			if time.Now().After(deadline) {
				runErr = fmt.Errorf("真实重探超时（stage=%d state=%s desc=%q）", stage, state, desc)
				app.Quit()
				return false
			}
			switch stage {
			case 0:
				if !u.loaded || !u.listSettled() {
					return true
				}
				if !u.pushSetup(legacyReprobeApp, "open", func(bool) {}) {
					runErr = fmt.Errorf("旧档案确认页没有推出来")
					app.Quit()
					return false
				}
				stage = 1
			case 1:
				v := u.activeSetup
				if v == nil || v.state != "confirm" {
					return true
				}
				if got := v.banner.GetButtonLabel(); got != u.tr("ui.setup.reads.reprobe.action", nil) {
					runErr = fmt.Errorf("旧档案横幅没有重探入口（%q）；FUSE 自检可用=%v",
						got, !u.fuseBlocked)
					app.Quit()
					return false
				}
				gobject.SignalEmitByName(&v.banner.Widget.Object, "button-clicked")
				stage = 2
			case 2:
				v := u.activeSetup
				if v == nil || v.state != "confirm" {
					return true
				}
				if v.readTrace == "" {
					return true
				}
				if v.readTrace != core.ReadTraceOK {
					// partial/unavailable 也算"拿到了非空结论"，但真实冒烟要求
					// 看到读证据升级；环境缺 strace 时如实报出来。
					runErr = fmt.Errorf("重探后 read_trace=%q（期望 ok）；请确认 strace 可用", v.readTrace)
					app.Quit()
					return false
				}
				for _, p := range v.profile.Paths {
					if strings.Contains(p, "Downloads") || strings.Contains(p, "Documents") {
						runErr = fmt.Errorf("重探把公共目录写进了隔离范围：%v", v.profile.Paths)
						app.Quit()
						return false
					}
				}
				if len(v.profile.Paths) < 2 {
					runErr = fmt.Errorf("重探没有覆盖到新观察到的数据目录：%v", v.profile.Paths)
					app.Quit()
					return false
				}
				gobject.SignalEmitByName(&v.confirmBtn.Widget.Object, "clicked")
				stage = 3
			case 3:
				if u.activeSetup != nil {
					return true
				}
				var err error
				reloaded, err = env.LoadProfile(legacyReprobeApp)
				if err != nil {
					runErr = fmt.Errorf("读取落盘档案失败：%v", err)
					app.Quit()
					return false
				}
				app.Quit()
				return false
			}
			return true
		}
		glib.TimeoutAdd(50, &tick, 0)
	}
	u.hold(&actcb)
	app.ConnectActivate(&actcb)
	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK 主循环退出码 %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}

	if reloaded.ReadTrace == "" {
		t.Fatalf("重探后档案仍没有读证据：%+v", reloaded)
	}
	if len(reloaded.Paths) < 2 {
		t.Fatalf("重探后档案只隔离了 %v（应包含新观察到的数据目录）", reloaded.Paths)
	}
	if entries, err := os.ReadDir(filepath.Join(env.Base, "instances", legacyReprobeApp)); err != nil || len(entries) == 0 {
		t.Fatalf("确认后没有创建实例目录：err=%v entries=%d", err, len(entries))
	}
	// 宿主零污染：探测/运行只写实例目录。bind 目标占位目录由产品按设计补建，
	// 但应用数据本身（脚本写/读的文件）绝不能出现在假 HOME 里。
	for _, f := range []string{".config/Demo/state", ".local/share/Demo/data"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			t.Fatalf("宿主 HOME 被写入了应用数据：%s", filepath.Join(home, f))
		}
	}
	t.Logf("真实重探完成：read_trace=%s paths=%v", reloaded.ReadTrace, reloaded.Paths)
}
