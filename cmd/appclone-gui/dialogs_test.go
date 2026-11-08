//go:build gtk_e2e

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

// TestGtkDialogResponses 覆盖删除/重命名的对话框响应路径。
//
// 本会话不能注入指针事件，对话框又只存在栈上；产品在 Choose 前把对象交给
// dialogHook（生产为 nil），测试拿它断言默认响应，并走**同一个** response
// 回调（u.cb.dialogFn）触发产品处理函数。这样覆盖的是真实接线，而不是
// 另写一份"模拟删除"。
func TestGtkDialogResponses(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "dialogs")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := core.Profile{
		App: "demo", Version: core.ProfileVersion, Exec: "/usr/bin/true",
		Paths: []string{"~/.config/Demo"}, ProbeMethod: "overlay",
		Ignored: core.DefaultIgnored(), Env: map[string]string{},
	}
	if _, err := env.CreateInstance(context.Background(), "demo", profile, "main"); err != nil {
		t.Fatal(err)
	}

	dialogs := make(chan *adw.AlertDialog, 4)
	oldHook := dialogHook
	dialogHook = func(d *adw.AlertDialog) { dialogs <- d }
	defer func() { dialogHook = oldHook }()

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkDialogs",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()
	u := &ui{app: app, env: env, lang: "zh_CN"}

	var runErr error
	fail := func(format string, a ...any) {
		if runErr == nil {
			runErr = fmt.Errorf(format, a...)
		}
		app.Quit()
	}
	take := func() *adw.AlertDialog {
		select {
		case d := <-dialogs:
			return d
		default:
			return nil
		}
	}

	phase := 0
	var cancelledAt time.Time
	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		u.buildWindow()
		deadline := time.Now().Add(30 * time.Second)
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if runErr != nil {
				return false
			}
			if time.Now().After(deadline) {
				fail("对话框回归超时（phase=%d）", phase)
				return false
			}
			if !u.loaded {
				return true
			}
			v := u.activeDetail
			if phase == 0 {
				if v == nil || !v.loaded {
					u.pushDetail("demo")
					return true
				}
				list, err := env.ListInstances("demo")
				if err != nil || len(list) != 1 {
					fail("应有 1 个实例：%v %v", list, err)
					return false
				}
				v.rename(list[0])
				d := take()
				if d == nil {
					fail("重命名没有经过 dialogHook")
					return false
				}
				defer d.Close()
				if got := d.GetDefaultResponse(); got != "cancel" {
					fail("重命名默认响应应为 cancel，得到 %q", got)
					return false
				}
				if !d.HasResponse("rename") {
					fail("重命名对话框缺少 rename 响应")
					return false
				}
				child := d.GetExtraChild()
				if child == nil {
					fail("重命名对话框缺少输入框")
					return false
				}
				gtk.EntryNewFromInternalPtr(child.GoPointer()).SetText("工作号")
				u.cb.dialogFn(*d, "rename")
				phase = 1
				return true
			}
			if phase == 1 {
				list, _ := env.ListInstances("demo")
				if len(list) == 1 && list[0].DisplayName == "工作号" {
					v.remove(list[0])
					d := take()
					if d == nil {
						fail("删除没有经过 dialogHook")
						return false
					}
					defer d.Close()
					if got := d.GetDefaultResponse(); got != "cancel" {
						fail("删除默认响应应为 cancel，得到 %q", got)
						return false
					}
					if got := d.GetResponseAppearance("trash"); got != adw.ResponseDestructiveValue {
						fail("trash 响应必须是 destructive，得到 %v", got)
						return false
					}
					// 先走一次取消：实例必须原样保留。
					u.cb.dialogFn(*d, "cancel")
					cancelledAt = time.Now()
					phase = 2
				}
				return true
			}
			if phase == 2 {
				if time.Since(cancelledAt) < 300*time.Millisecond {
					return true
				}
				if list, _ := env.ListInstances("demo"); len(list) != 1 {
					fail("取消删除后实例不应消失，现有 %d 个", len(list))
					return false
				}
				// 第二次真正删除：确认后必须进回收站（实例目录消失）。
				list, err := env.ListInstances("demo")
				if err != nil || len(list) != 1 {
					fail("删除前实例状态异常：%v %v", list, err)
					return false
				}
				v.remove(list[0])
				d := take()
				if d == nil {
					fail("第二次删除没有经过 dialogHook")
					return false
				}
				defer d.Close()
				u.cb.dialogFn(*d, "trash")
				phase = 3
				return true
			}
			list, err := env.ListInstances("demo")
			if err != nil {
				fail("列出实例失败：%v", err)
				return false
			}
			if len(list) == 0 {
				app.Quit()
				return false
			}
			return true
		}
		glib.TimeoutAdd(50, &tick, 0)
	}
	app.ConnectActivate(&actcb)
	u.hold(&actcb)

	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK 主循环退出码 %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
}
