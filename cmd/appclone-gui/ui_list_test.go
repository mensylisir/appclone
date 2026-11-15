//go:build linux && gtk_e2e

package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"
	"github.com/mensylisir/appclone/core"
)

func TestGtkListBatches(t *testing.T) {
	env, _ := gtkTestEnv(t, "list-batches")
	app := adw.NewApplication("io.github.mensylisir.Appclone.ListBatches", gio.GApplicationNonUniqueValue)
	defer app.Unref()
	u := newUI(app, env)
	deadline := time.Now().Add(20 * time.Second)
	step, generations := 0, 0
	keyChecked := false
	var runErr error
	var focus uintptr
	var scroll *gtk.Adjustment
	var scrollValue float64
	fail := func(format string, args ...any) { runErr = fmt.Errorf(format, args...); app.Quit() }
	var tick glib.SourceFunc
	tick = func(uintptr) bool {
		if time.Now().After(deadline) {
			fail("list lifecycle timed out at %d", step)
			return false
		}
		if u.closed {
			return true
		} // Application.Hold must be released by the product.
		if !u.loaded || !u.envChecked || !u.win.GetMapped() {
			return true
		}
		switch step {
		case 0:
			if !u.listSettled() {
				return true
			}
			u.pollPaused = true
			u.apps = nil
			for i := 0; i < 200; i++ {
				u.apps = append(u.apps, core.App{Key: fmt.Sprintf("clone%03d", i), Name: fmt.Sprintf("Clone %03d", i), Icon: "folder", InstanceN: 1})
				u.apps = append(u.apps, core.App{Key: fmt.Sprintf("opaque%03d", i), Name: fmt.Sprintf("应用 %03d", i), EnglishName: fmt.Sprintf("Editor %03d", i), Icon: "folder"})
			}
			u.expanded = true
			u.stack.SetVisibleChildName("all")
			u.renderList()
			if len(u.listRows) == 0 || len(u.listRows) > 4 || len(listRowTitles(u.onPage)) != 0 {
				fail("first batch did not prioritize visible all-apps page")
				return false
			}
			// A burst of renders must reuse the queued idle instead of spawning one
			// per keystroke; retirement stays bounded per generation.
			if !u.listIdlePending {
				fail("first batch did not leave a queued list idle")
				return false
			}
			spawns := u.listIdleSpawns
			retireBound := len(u.retiredRows) + len(u.listRows) + 4*11
			for i := 0; i < 10; i++ {
				u.renderList()
			}
			if u.listIdleSpawns != spawns {
				fail("burst of renders spawned %d idles (had %d)", u.listIdleSpawns, spawns)
				return false
			}
			if !u.listIdlePending {
				fail("burst cleared the queued list idle")
				return false
			}
			if u.retiredGroupCount != len(u.retiredGroups) {
				fail("retiredGroupCount=%d but %d groups", u.retiredGroupCount, len(u.retiredGroups))
				return false
			}
			if len(u.retiredGroups) > len(u.retiredRows) {
				fail("%d retired groups for %d rows", len(u.retiredGroups), len(u.retiredRows))
				return false
			}
			if len(u.retiredRows) > retireBound {
				fail("burst accumulated %d retired rows (bound %d)", len(u.retiredRows), retireBound)
				return false
			}
			for _, g := range u.retiredGroups {
				if !g.detached && g.widget.GetVisible() {
					fail("retired group stayed visible")
					return false
				}
			}
			u.stack.SetVisibleChildName("on")
			step++
		case 1:
			if len(listRowTitles(u.onPage)) == 0 {
				return true
			}
			if u.listBuild == nil {
				fail("hidden page finished before tab switch could be exercised")
				return false
			}
			u.stack.SetVisibleChildName("all")
			u.search.GrabFocus()
			u.search.SetText("editor 0")
			u.applySearch()
			gen, build := u.listGen, u.listBuild
			u.search.SetText("EDITOR 0")
			u.applySearch()
			if u.listGen != gen || u.listBuild != build {
				fail("same query cancelled a valid batch")
				return false
			}
			retirementBound := len(u.retiredRows) + len(u.listRows) + 4
			for i := 0; i < 40; i++ {
				u.search.SetText(fmt.Sprintf("editor %03d", i))
				u.applySearch()
			}
			if len(u.retiredRows) > retirementBound {
				fail("rapid changes accumulated %d retired rows", len(u.retiredRows))
				return false
			}
			step++
		case 2:
			if !u.listSettled() {
				return true
			}
			if len(u.retiredRows) != 0 || len(u.retiredGroups) != 0 {
				fail("settled list still tracks retired rows/groups")
				return false
			}
			if !keyChecked {
				titles := listRowTitles(u.offPage)
				if len(titles) != 1 || titles[0] != "应用 039" {
					fail("stale rows or failed English search: %v", titles)
					return false
				}
				keyChecked = true
				u.search.SetText("opaque039")
				u.applySearch()
				return true
			}
			titles := listRowTitles(u.offPage)
			if len(titles) != 1 || titles[0] != "应用 039" {
				fail("key search failed: %v", titles)
				return false
			}
			u.search.SetText("")
			u.applySearch()
			step++
		case 3:
			if !u.listSettled() {
				return true
			}
			if len(listRowTitles(u.offPage)) != 200 {
				fail("expanded list incomplete")
				return false
			}
			// Focus inside a group that is about to be hidden must move to the
			// visible scroll page on this generation's first batch, not be lost.
			if len(u.listRows) == 0 {
				fail("no row to focus")
				return false
			}
			u.win.SetFocus(u.listRows[0].widget)
			ff := u.win.GetFocus()
			if ff == nil {
				fail("could not establish focus inside a list group")
				return false
			}
			inside := false
			for _, g := range u.listGroups {
				if ff.GoPointer() == g.widget.GoPointer() || ff.IsAncestor(&g.widget.Widget) {
					inside = true
				}
			}
			if !inside {
				fail("focus landed on %s, not inside a list group (%d groups)", typeNameOf(ff), len(u.listGroups))
				return false
			}
			ff.Unref()
			u.search.SetText("editor")
			u.applySearch()
			step++
		case 4:
			if !u.listSettled() {
				return true
			}
			if u.listRefocus {
				fail("focus handoff marker not consumed")
				return false
			}
			hf := u.win.GetFocus()
			if hf == nil || (hf.GoPointer() != u.offPage.GoPointer() && !hf.IsAncestor(&u.offPage.Widget)) {
				fail("focus was not handed to the visible all-apps page")
				return false
			}
			hf.Unref()
			u.search.GrabFocus()
			f := u.win.GetFocus()
			if f == nil {
				fail("search has no focus")
				return false
			}
			focus = f.GoPointer()
			f.Unref()
			for _, w := range findWidgets(&u.offPage.Widget, func(w *gtk.Widget) bool { return typeNameOf(w) == "GtkScrolledWindow" }) {
				if scroll == nil {
					scroll = gtk.ScrolledWindowNewFromInternalPtr(w.GoPointer()).GetVadjustment()
				}
			}
			if scroll == nil {
				fail("scroll adjustment missing")
				return false
			}
			scroll.SetValue(100)
			scrollValue = scroll.GetValue()
			generations = u.listGen
			u.search.SetText("edit")
			u.applySearch()
			step++
		case 5:
			if !u.listSettled() {
				return true
			}
			f := u.win.GetFocus()
			if f == nil {
				fail("focus lost")
				return false
			}
			gotFocus := f.GoPointer()
			f.Unref()
			if u.listGen != generations || gotFocus != focus || scroll.GetValue() != scrollValue {
				fail("same results changed generation/focus/scroll")
				return false
			}
			scroll.Unref()
			scroll = nil
			// Environment refresh may supersede an active render. The new rows
			// must all adopt the resulting state, even those made in later batches.
			u.search.SetText("")
			u.applySearch()
			u.fatal = true
			u.renderList()
			step++
		case 6:
			if !u.listSettled() {
				return true
			}
			for _, r := range u.listRows {
				if typeNameOf(r.widget) == "AdwSwitchRow" && r.widget.GetSensitive() {
					fail("late row escaped fatal state")
					return false
				}
			}
			u.fatal = false
			u.renderList()
			u.busy = 1
			u.win.Close()
			if u.closed {
				fail("transactional close invalidated the list")
				return false
			}
			u.busy = 0
			step++
		case 7:
			if !u.listSettled() {
				return true
			}
			u.renderList()
			step++
			u.win.Close()
			if !u.closed || !u.listCloseHold {
				fail("last-window close did not hold pending reclamation")
				return false
			}
			gen := u.listGen
			u.applySearch()
			u.reload()
			u.refreshRunning()
			u.checkEnv()
			u.renderList()
			if u.listGen != gen {
				fail("closed UI accepted a new render")
				return false
			}
		}
		return true
	}
	activate := func(_ gio.Application) { u.buildWindow(); glib.TimeoutAdd(5, &tick, 0) }
	app.ConnectActivate(&activate)
	u.hold(&activate, &tick)
	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK exited %d", code)
	}
	if scroll != nil {
		scroll.Unref()
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if step != 8 || !u.closed || !u.listSettled() || u.listCloseHold {
		t.Fatalf("incomplete close/reclaim: step=%d closed=%t settled=%t hold=%t", step, u.closed, u.listSettled(), u.listCloseHold)
	}
	u.cb.mu.Lock()
	defer u.cb.mu.Unlock()
	if len(u.cb.switchN) != 0 || len(u.cb.action) != 0 {
		t.Fatal("retired list handlers survived close")
	}
}

// Focus handoff follows actual page visibility, not the stack's remembered
// child: when the last clone disappears the empty page replaces onPage, and if
// every page is hidden the search box is the final fallback.
func TestGtkFocusTargets(t *testing.T) {
	step := 0
	runGtkBody(t, "focus-targets", func(u *ui) (bool, error) {
		u.pollPaused = true
		switch step {
		case 0:
			if !u.listSettled() {
				return false, nil
			}
			u.apps = []core.App{{Key: "clone000", Name: "Clone 000", Icon: "folder", InstanceN: 1}}
			u.expanded = true
			u.stack.SetVisibleChildName("on")
			u.renderList()
			step++
		case 1:
			if !u.listSettled() {
				return false, nil
			}
			if len(u.listRows) == 0 {
				return false, fmt.Errorf("no clone row to focus")
			}
			u.win.SetFocus(u.listRows[0].widget)
			if f := u.win.GetFocus(); f == nil {
				return false, fmt.Errorf("could not focus the clone row")
			} else {
				f.Unref()
			}
			u.apps = nil // last clone removed: emptyPage must replace onPage
			u.renderList()
			step++
		default:
			if !u.listSettled() {
				return false, nil
			}
			if u.listRefocus {
				return false, fmt.Errorf("focus handoff marker not consumed")
			}
			if u.onPage.GetVisible() || !u.emptyPage.GetVisible() {
				return false, fmt.Errorf("empty state did not replace the clones page")
			}
			hf := u.win.GetFocus()
			if hf == nil || hf.GoPointer() != u.emptyPick.GoPointer() {
				return false, fmt.Errorf("focus did not move to the empty-state pick button")
			}
			hf.Unref()
			u.onPage.SetVisible(false)
			u.emptyPage.SetVisible(false)
			u.offPage.SetVisible(false)
			u.focusCurrentView()
			sf := u.win.GetFocus()
			if sf == nil || (sf.GoPointer() != u.search.GoPointer() && !sf.IsAncestor(&u.search.Widget)) {
				return false, fmt.Errorf("focus fallback did not use the search box")
			}
			sf.Unref()
			return true, nil
		}
		return false, nil
	})
}

// A generation whose build produces no row never reaches the first-batch focus
// handoff. The marker captured from the hidden generation must be dropped when
// the build ends, or the next unrelated render steals focus.
func TestGtkListEmptyBatchClearsFocusMarker(t *testing.T) {
	runGtkBody(t, "list-empty-marker", func(u *ui) (bool, error) {
		u.pollPaused = true
		if !u.listSettled() {
			return false, nil
		}
		u.listGen++
		u.listRefocus = true
		u.listBuild = &listBuild{gen: u.listGen, view: appList{}, addDone: true, tailDone: true, firstBatch: true}
		u.listBatch()
		if u.listBuild != nil {
			return false, fmt.Errorf("empty build was not finished")
		}
		if u.listRefocus {
			return false, fmt.Errorf("focus handoff marker leaked out of an empty generation")
		}
		return true, nil
	})
}

// listSettled means no build is pending, every retired row/group is gone and no
// list idle task is still queued. Tests poll it instead of sleeping.
func (u *ui) listSettled() bool {
	return u.listDrained() && !u.listIdlePending
}
