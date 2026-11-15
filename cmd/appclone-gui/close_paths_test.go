//go:build linux && gtk_e2e

package main

// 关窗路径：在途详情刷新、探测页、列表退休必须被放弃/限定排空，
// 不能对着已销毁的控件操作，也不能让 Hold 悬挂。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/mensylisir/appclone/core"
)

// runUntilClosed drives body until it reports done, then lets the process exit
// on its own: no app.Quit, so a missing Hold/Release or a stuck list drain makes
// Run hang and the test fail instead of passing on a forced quit.
func runUntilClosed(t *testing.T, name string, env *core.Env, body func(u *ui) (bool, error)) {
	t.Helper()
	if env == nil {
		env, _ = gtkTestEnv(t, name)
	}
	app := adw.NewApplication("io.github.mensylisir.Appclone.Close"+name,
		gio.GApplicationNonUniqueValue)
	defer app.Unref()
	u := newUI(app, env)
	var runErr error
	deadline := time.Now().Add(60 * time.Second)
	var tick glib.SourceFunc
	tick = func(uintptr) bool {
		if runErr != nil {
			return false
		}
		if time.Now().After(deadline) {
			runErr = fmt.Errorf("%s hung before draining (closed=%t hold=%t)", name, u.closed, u.listCloseHold)
			app.Quit()
			return false
		}
		if !u.loaded || !u.envChecked || (!u.win.GetMapped() && !u.closed) {
			return true
		}
		// Fixtures are static: the product poll could replace the app list or
		// detail state mid-assertion, so pause it for deterministic close paths.
		u.pollPaused = true
		done, err := body(u)
		if err != nil {
			runErr = err
			app.Quit()
			return false
		}
		return !done
	}
	activate := func(_ gio.Application) { u.buildWindow(); glib.TimeoutAdd(5, &tick, 0) }
	app.ConnectActivate(&activate)
	u.hold(&activate, &tick)
	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK exited %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !u.closed || !u.listSettled() || u.listCloseHold {
		t.Fatalf("close did not settle: closed=%t drained=%t hold=%t", u.closed, u.listDrained(), u.listCloseHold)
	}
	u.cb.mu.Lock()
	defer u.cb.mu.Unlock()
	if len(u.cb.switchN) != 0 || len(u.cb.action) != 0 || len(u.cb.dialog) != 0 || len(u.cb.idle) != 0 {
		t.Fatalf("handlers survived close: switch=%d action=%d dialog=%d idle=%d",
			len(u.cb.switchN), len(u.cb.action), len(u.cb.dialog), len(u.cb.idle))
	}
	if len(u.pages) != 0 {
		t.Fatalf("%d pushed pages still registered after close", len(u.pages))
	}
}

// TestGtkListRetireClose starts a search that leaves old groups hidden and rows
// queued, then closes the window: every tracked group must be detached and the
// held drain must finish, leaving slices empty.
func TestGtkListRetireClose(t *testing.T) {
	phase := 0
	runUntilClosed(t, "ListRetireClose", nil, func(u *ui) (bool, error) {
		switch phase {
		case 0:
			u.apps = nil
			for i := 0; i < 120; i++ {
				u.apps = append(u.apps, core.App{Key: fmt.Sprintf("app%03d", i),
					Name: fmt.Sprintf("应用 %03d", i), Icon: "folder"})
			}
			u.expanded = true
			u.stack.SetVisibleChildName("all")
			u.renderList()
			phase = 1
			return false, nil
		case 1:
			if !u.listSettled() || u.prevRowCount == 0 {
				return false, nil
			}
			u.search.SetText("应用 0")
			u.applySearch()
			phase = 2
			return false, nil
		}
		if u.retiredGroupCount != len(u.retiredGroups) {
			return false, fmt.Errorf("retiredGroupCount=%d but %d groups tracked", u.retiredGroupCount, len(u.retiredGroups))
		}
		if len(u.retiredGroups) > len(u.retiredRows) {
			return false, fmt.Errorf("%d groups for %d rows", len(u.retiredGroups), len(u.retiredRows))
		}
		for _, g := range u.retiredGroups {
			if !g.detached && g.widget.GetVisible() {
				return false, fmt.Errorf("retired group still visible before close")
			}
		}
		u.win.Close()
		if !u.closed {
			return false, fmt.Errorf("close request was not accepted")
		}
		for _, g := range u.retiredGroups {
			if !g.detached {
				return false, fmt.Errorf("group left attached to a dying page")
			}
		}
		if len(u.retiredRows) == 0 {
			return false, fmt.Errorf("no rows left for the close drain")
		}
		return true, nil
	})
}

func detailFixture(t *testing.T) (*core.Env, string) {
	t.Helper()
	// Under design/guitest-gtk.sh APPCLONE_GTK_ROOT is shared, so the env name
	// must be unique per test: several close-path tests create the same "main"
	// instance and would otherwise collide across tests in one process.
	env, appsDir := gtkTestEnv(t, strings.ToLower(t.Name()))
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
	return env, appsDir
}

// TestGtkDetailOpenClose closes with an in-flight detail refresh and a detail
// page on the navigation stack: abandon must clear handlers without popping or
// touching destroyed widgets.
func TestGtkDetailOpenClose(t *testing.T) {
	env, _ := detailFixture(t)
	runUntilClosed(t, "DetailOpenClose", env, func(u *ui) (bool, error) {
		if u.activeDetail == nil {
			u.pushDetail("demo")
			return false, nil
		}
		v := u.activeDetail
		if !v.loaded {
			return false, nil
		}
		v.refresh()
		u.win.Close()
		if !u.closed {
			return false, fmt.Errorf("close request was not accepted")
		}
		if u.activeDetail != nil {
			return false, fmt.Errorf("activeDetail survived close")
		}
		u.cb.mu.Lock()
		dialogs := len(u.cb.dialog)
		u.cb.mu.Unlock()
		if dialogs != 0 {
			return false, fmt.Errorf("%d dialog handlers survived close", dialogs)
		}
		return true, nil
	})
}

// TestGtkRenameDialogClose leaves the rename dialog unanswered, closes the
// window, then delivers the response anyway: the cleared handler must ignore it
// without reading the entry, and no rename may reach the instance on disk.
func TestGtkRenameDialogClose(t *testing.T) {
	env, _ := detailFixture(t)
	dialogs := make(chan *adw.AlertDialog, 2)
	oldHook := dialogHook
	dialogHook = func(d *adw.AlertDialog) { dialogs <- d }
	defer func() { dialogHook = oldHook }()
	var dlg *adw.AlertDialog
	origName := ""
	closeTries := 0
	runUntilClosed(t, "RenameDialogClose", env, func(u *ui) (bool, error) {
		if u.closed {
			// A close may be processed on a later tick; never push again here.
			if dlg == nil {
				return false, fmt.Errorf("window closed before the rename dialog opened")
			}
			u.cb.mu.Lock()
			pending := len(u.cb.dialog)
			u.cb.mu.Unlock()
			if pending != 0 {
				return false, fmt.Errorf("%d dialog handlers survived close", pending)
			}
			u.cb.dialogFn(*dlg, "rename")
			return true, nil
		}
		if u.activeDetail == nil {
			u.pushDetail("demo")
			return false, nil
		}
		v := u.activeDetail
		if !v.loaded || len(v.insts) == 0 {
			return false, nil
		}
		if dlg == nil {
			origName = v.insts[0].DisplayName
			v.rename(v.insts[0])
			select {
			case dlg = <-dialogs:
			default:
				return false, fmt.Errorf("rename dialog did not reach dialogHook")
			}
		}
		// A modal dialog can defer the parent's close request by a frame.
		closeTries++
		u.win.Close()
		if closeTries > 10 {
			return false, fmt.Errorf("close request was not accepted")
		}
		return false, nil
	})
	list, err := env.ListInstances("demo")
	if err != nil || len(list) != 1 {
		t.Fatalf("instances after closed rename: %v %v", list, err)
	}
	if list[0].DisplayName != origName {
		t.Fatalf("closed dialog renamed the instance to %q (was %q)", list[0].DisplayName, origName)
	}
}

// TestGtkRenameConfirmedClose confirms a rename and closes in the same tick:
// the transaction still completes on disk (only its UI feedback is skipped),
// and the Hold keeps the process alive until the idle releases it.
func TestGtkRenameConfirmedClose(t *testing.T) {
	env, _ := detailFixture(t)
	dialogs := make(chan *adw.AlertDialog, 2)
	oldHook := dialogHook
	dialogHook = func(d *adw.AlertDialog) { dialogs <- d }
	defer func() { dialogHook = oldHook }()
	var dlg *adw.AlertDialog
	closeTries := 0
	runUntilClosed(t, "RenameConfirmedClose", env, func(u *ui) (bool, error) {
		if u.closed {
			if dlg == nil {
				return false, fmt.Errorf("window closed before the rename dialog opened")
			}
			return true, nil
		}
		if u.activeDetail == nil {
			u.pushDetail("demo")
			return false, nil
		}
		v := u.activeDetail
		if !v.loaded || len(v.insts) == 0 {
			return false, nil
		}
		if dlg == nil {
			v.rename(v.insts[0])
			select {
			case dlg = <-dialogs:
			default:
				return false, fmt.Errorf("rename dialog did not reach dialogHook")
			}
			child := dlg.GetExtraChild()
			if child == nil {
				return false, fmt.Errorf("rename dialog has no entry")
			}
			gtk.EntryNewFromInternalPtr(child.GoPointer()).SetText("工作号")
			u.cb.dialogFn(*dlg, "rename")
		}
		closeTries++
		u.win.Close()
		if closeTries > 10 {
			return false, fmt.Errorf("close request was not accepted")
		}
		return false, nil
	})
	list, err := env.ListInstances("demo")
	if err != nil || len(list) != 1 {
		t.Fatalf("instances after confirmed rename: %v %v", list, err)
	}
	if list[0].DisplayName != "工作号" {
		t.Fatalf("confirmed rename did not land after close: %q", list[0].DisplayName)
	}
}

// TestGtkStopThenClose answers the stop dialog and closes the window while
// StopRun is in flight. Run must not return before the transaction's idle has
// released its Hold; runUntilClosed would otherwise hang or time out.
func TestGtkStopThenClose(t *testing.T) {
	env, _ := detailFixture(t)
	dialogs := make(chan *adw.AlertDialog, 2)
	oldHook := dialogHook
	dialogHook = func(d *adw.AlertDialog) { dialogs <- d }
	defer func() { dialogHook = oldHook }()
	var dlg *adw.AlertDialog
	closeTries := 0
	runUntilClosed(t, "StopThenClose", env, func(u *ui) (bool, error) {
		if u.closed {
			if dlg == nil {
				return false, fmt.Errorf("window closed before the stop dialog opened")
			}
			return true, nil
		}
		if u.activeDetail == nil {
			u.pushDetail("demo")
			return false, nil
		}
		v := u.activeDetail
		if !v.loaded || len(v.insts) == 0 {
			return false, nil
		}
		if dlg == nil {
			v.stop(v.insts[0])
			select {
			case dlg = <-dialogs:
			default:
				return false, fmt.Errorf("stop dialog did not reach dialogHook")
			}
			u.cb.dialogFn(*dlg, "stop")
		}
		closeTries++
		u.win.Close()
		if closeTries > 10 {
			return false, fmt.Errorf("close request was not accepted")
		}
		return false, nil
	})
}

// TestGtkClosedIgnoresCalls verifies every closed-guarded entry point called
// after the window closed neither touches widgets nor queues new idle work.
func TestGtkClosedIgnoresCalls(t *testing.T) {
	env, _ := detailFixture(t)
	runUntilClosed(t, "ClosedCalls", env, func(u *ui) (bool, error) {
		if u.activeDetail == nil {
			u.pushDetail("demo")
			return false, nil
		}
		v := u.activeDetail
		if !v.loaded {
			return false, nil
		}
		u.win.Close()
		if !u.closed {
			return false, fmt.Errorf("close request was not accepted")
		}
		u.cb.mu.Lock()
		before := len(u.cb.idle)
		u.cb.mu.Unlock()
		u.toastMsg("after close")
		v.load(true)
		v.refresh()
		u.applySearch()
		u.renderList()
		u.reload()
		u.cb.mu.Lock()
		after := len(u.cb.idle)
		u.cb.mu.Unlock()
		if after != before {
			return false, fmt.Errorf("closed UI queued %d new idle task(s)", after-before)
		}
		return true, nil
	})
}

// TestGtkSetupClose closes while the probe page is active (not creating).
func TestGtkSetupClose(t *testing.T) {
	env, appsDir := gtkTestEnv(t, "setup-close")
	if err := os.WriteFile(filepath.Join(appsDir, "demo.desktop"),
		[]byte("[Desktop Entry]\nType=Application\nName=Demo\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runUntilClosed(t, "SetupClose", env, func(u *ui) (bool, error) {
		if u.activeSetup == nil {
			if !u.pushSetup("demo", "setup", nil) {
				return false, fmt.Errorf("pushSetup refused")
			}
			return false, nil
		}
		v := u.activeSetup
		if v.state == "creating" {
			return false, fmt.Errorf("unexpected creating state in close fixture")
		}
		u.win.Close()
		if !u.closed {
			return false, fmt.Errorf("close request was not accepted")
		}
		if u.activeSetup != nil {
			return false, fmt.Errorf("activeSetup survived close")
		}
		if v.fixed != nil || v.scope != nil {
			return false, fmt.Errorf("setup scopes not closed on abandon")
		}
		return true, nil
	})
}
