package main

import (
	"fmt"
	"os"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gtk"
)

const (
	listBatchBudget    = 8 * time.Millisecond
	closeBatchBudget   = 16 * time.Millisecond
	closeRowsPerBatch  = 64
	normalRowsPerBatch = 4
	retireRowsPerBatch = 2
)

// Groups retain their constructor reference until their last retired row has
// been removed. Each row owns only its own subtree; handlers belong to listScope.
type listGroup struct {
	widget   *adw.PreferencesGroup
	page     *adw.PreferencesPage
	rows     int
	detached bool
}

type listRow struct {
	group  *listGroup
	widget *gtk.Widget
	scope  *renderScope
}

type listBuild struct {
	gen               int
	view              appList
	on, add, off      *listGroup
	onNext, offNext   int
	addDone, tailDone bool
	firstBatch        bool
}

type listMeasurement struct {
	Prepare          bool
	Duration         time.Duration
	Created, Retired int
}

// renderList invalidates old handlers before detaching anything. Destruction
// and creation run in bounded batches so typing can supersede an unfinished list.
func (u *ui) renderList() {
	if u.closed {
		return
	}
	started := time.Now()
	u.retireList()
	view := projectAppList(u.apps, u.query, u.expanded)
	b := &listBuild{gen: u.listGen, view: view, addDone: len(view.on) == 0,
		tailDone: len(view.off) > 0 && view.tail == "", firstBatch: true}
	u.listBuild = b
	u.listScope = &renderScope{c: u.cb}
	u.toggled = make(map[toggleMark]bool)
	empty := len(view.on) == 0
	u.emptyPage.SetVisible(empty)
	u.onPage.SetVisible(!empty)
	if u.onPageObj != nil {
		u.onPageObj.SetBadgeNumber(uint32(len(view.on)))
	}
	if !empty {
		b.on = u.newListGroup(true)
		b.on.widget.SetTitle(u.tr("ui.group.clones.title", nil))
		b.on.widget.SetDescription(u.tr("ui.group.clones.desc", nil))
		b.add = u.newListGroup(true)
	}
	b.off = u.newListGroup(false)
	b.off.widget.SetDescription(u.tr("ui.group.all.desc", nil))
	if u.listObserve != nil {
		u.listObserve(listMeasurement{Prepare: true, Duration: time.Since(started)})
	}
	// A queued callback reads the latest build, so a burst of queries adds no
	// extra idle sources and never keeps closures holding obsolete projections.
	if !u.listIdlePending {
		u.listBatch()
	}
}

func (u *ui) newListGroup(on bool) *listGroup {
	g := &listGroup{widget: adw.NewPreferencesGroup()}
	if on {
		g.page = u.onPage
	} else {
		g.page = u.offPage
	}
	g.page.Add(g.widget)
	u.listGroups = append(u.listGroups, g)
	u.groups = append(u.groups, g.widget)
	return g
}

func (u *ui) retireList() {
	// Sampling here keeps the test bound meaningful even when generations
	// overlap: the assertion uses the count captured for this generation.
	u.prevRowCount = len(u.listRows)
	u.listGen++
	u.listBuild = nil
	if u.listScope != nil {
		u.listScope.close()
		u.listScope = nil
	}
	// Focus inside a group that is about to be hidden is dropped by GTK. Record
	// it before SetVisible(false); the first batch of the new generation hands
	// focus to the visible view once.
	if !u.listRefocus && u.win != nil {
		if f := u.win.GetFocus(); f != nil {
			// GetFocus takes its own reference; release it once the ancestor test
			// is done or every render pins the focused widget for good.
			for _, g := range u.listGroups {
				if f.GoPointer() == g.widget.GoPointer() || f.IsAncestor(&g.widget.Widget) {
					u.listRefocus = true
					break
				}
			}
			f.Unref()
		}
	}
	// Detaching a full group is O(rows) (unparent + listbox bookkeeping) and was
	// the 37ms search stall. Hide it instead: no input, no duplicate rows, and
	// rows retire in the same bounded batches. An empty group is detached and
	// unreffed here; a non-empty one is detached by its last retired row (or by
	// closeList before the window goes away).
	for _, g := range u.listGroups {
		if g.rows == 0 {
			g.page.Remove(g.widget)
			g.widget.Unref()
			g.detached = true
			continue
		}
		g.widget.SetVisible(false)
		u.retiredGroupCount++
		u.retiredGroups = append(u.retiredGroups, g)
	}
	u.retiredRows = append(u.retiredRows, u.listRows...)
	u.listRows, u.listGroups = nil, nil
	u.groups = nil
}

func (u *ui) retireOneRow() {
	r := u.retiredRows[0]
	u.retiredRows[0] = listRow{}
	u.retiredRows = u.retiredRows[1:]
	r.group.widget.Remove(r.widget)
	r.scope.close()
	r.group.rows--
	if r.group.rows == 0 {
		// Always leave the tracking list, even when the close path already
		// detached the group: a stale pointer would be Remove'd after Unref.
		if !r.group.detached {
			r.group.page.Remove(r.group.widget)
		}
		r.group.detached = true
		r.group.widget.Unref()
		u.retiredGroupCount--
		for i := range u.retiredGroups {
			if u.retiredGroups[i] == r.group {
				last := len(u.retiredGroups) - 1
				u.retiredGroups[i] = u.retiredGroups[last]
				u.retiredGroups[last] = nil
				u.retiredGroups = u.retiredGroups[:last]
				break
			}
		}
	}
	if len(u.retiredRows) == 0 {
		u.retiredRows = nil
	}
}

func (u *ui) listBatch() {
	started := time.Now()
	created, retired := 0, 0
	firstBatchCreated := false
	hadRetired := len(u.retiredRows) > 0

	retireLimit := retireRowsPerBatch
	budget := listBatchBudget
	if u.closed {
		// No user input can race the close path; drain with larger batches so
		// the process exits promptly. The first operation always runs.
		retireLimit = closeRowsPerBatch
		budget = closeBatchBudget
	}
	for retired < retireLimit && len(u.retiredRows) > 0 &&
		(retired == 0 || time.Since(started) < budget) {
		u.retireOneRow()
		retired++
	}

	createLimit := normalRowsPerBatch
	if hadRetired {
		// Keep the retired queue bounded: never create more rows than this batch
		// reclaimed. The close path creates nothing.
		createLimit = retired
	}
	if !u.closed {
		for u.listBuild != nil && created < createLimit &&
			(created+retired == 0 || time.Since(started) < budget) {
			b := u.listBuild
			if b.gen != u.listGen {
				break
			}
			on := u.stack.GetVisibleChildName() == "on"
			if !u.nextListRow(b, on) && !u.nextListRow(b, !on) {
				u.listBuild = nil
				break
			}
			if b.firstBatch {
				b.firstBatch = false
				firstBatchCreated = true
			}
			created++
			if b.onNext == len(b.view.on) && b.offNext == len(b.view.off) && b.addDone && b.tailDone {
				u.listBuild = nil
			}
		}
	}
	if !u.closed && created > 0 {
		u.stack.SetVisible(true)
		u.switcher.SetSensitive(true)
	}
	if firstBatchCreated {
		// Consume the focus marker exactly once, on this generation's first
		// created batch. Later batches must not steal focus from the search box.
		if u.listRefocus {
			u.listRefocus = false
			u.focusCurrentView()
		}
	}
	if u.listBuild == nil && len(u.retiredRows) == 0 {
		// A generation can end without ever creating a row (empty result): the
		// first-batch handoff above never runs, so the marker must not leak into
		// the next, unrelated render and steal focus there.
		u.listRefocus = false
	}
	if u.listObserve != nil {
		u.listObserve(listMeasurement{Duration: time.Since(started), Created: created, Retired: retired})
	}
	if u.listBuild != nil || len(u.retiredRows) > 0 {
		u.scheduleListBatch()
		return
	}
	if len(u.retiredGroups) > 0 {
		// Invariant breach: groups without rows can never retire by themselves.
		// Clear them instead of scheduling empty batches forever.
		fmt.Fprintf(os.Stderr, "appclone-gui: %d retired group(s) without rows\n", len(u.retiredGroups))
		u.retiredGroups = nil
		u.retiredGroupCount = 0
	}
	if u.listCloseHold {
		u.listCloseHold = false
		u.app.Release()
	}
}

func (u *ui) scheduleListBatch() {
	if u.listIdlePending {
		return
	}
	u.listIdlePending = true
	u.listIdleSpawns++
	u.goIdle(func(uintptr) bool {
		u.listIdlePending = false
		u.listBatch()
		return false
	})
}

// Every tail action is a row too and consumes the same batch allowance.
func (u *ui) nextListRow(b *listBuild, on bool) bool {
	sc := &renderScope{c: u.cb}
	var row *gtk.Widget
	g := b.off
	if on {
		g = b.on
		if b.onNext < len(b.view.on) {
			row = &u.newAppRow(b.view.on[b.onNext], true, sc).Widget
			b.onNext++
		} else if !b.addDone {
			g = b.add
			a := adw.NewActionRow()
			sc.own(&a.Widget)
			a.SetTitle(u.tr("ui.row.add_clone.title", nil))
			a.SetSubtitle(u.tr("ui.row.add_clone.subtitle", nil))
			ic := u.symbolicIcon("list-add-symbolic")
			sc.own(&ic.Widget)
			a.AddPrefix(&ic.Widget)
			a.SetActivatable(true)
			sc.row(a, func() { u.stack.SetVisibleChildName("all") })
			row, b.addDone = &a.Widget, true
		}
	} else if b.offNext < len(b.view.off) {
		row = &u.newAppRow(b.view.off[b.offNext], false, sc).Widget
		b.offNext++
	} else if !b.tailDone {
		if b.view.tail == "more" {
			row = &u.moreRow(u.tr("ui.row.show_all", map[string]any{"n": b.view.total}), sc).Widget
		} else {
			a := adw.NewActionRow()
			sc.own(&a.Widget)
			switch b.view.tail {
			case "limited":
				a.SetTitle(u.tr("ui.row.search_limit", map[string]any{"shown": searchLimit, "total": b.view.total}))
			case "no_match":
				a.SetTitle(u.tr("ui.row.no_match", map[string]any{"query": u.query}))
			default:
				a.SetTitle(u.tr("ui.row.all_clones", nil))
			}
			a.SetSensitive(false)
			row = &a.Widget
		}
		b.tailDone = true
	}
	if row == nil {
		return false
	}
	// Handlers are invalidated immediately on the next generation. Ownership
	// stays with this row until it is physically removed from its old group.
	u.listScope.keys = append(u.listScope.keys, sc.keys...)
	sc.keys = nil
	g.widget.Add(row)
	g.rows++
	u.listRows = append(u.listRows, listRow{group: g, widget: row, scope: sc})
	return true
}

func (u *ui) closeList() {
	if u.closed {
		return
	}
	u.closed = true
	// retireList advances the generation, clears the build/scope, queues rows
	// and records focus. Everything after this only detaches groups, so the
	// counters are not touched twice.
	u.retireList()
	// The window is about to be destroyed: detach every group still on a page,
	// including groups from earlier generations that are still draining. Rows
	// are removed from detached groups afterwards; Unref is left to the last row.
	for _, g := range u.retiredGroups {
		if g.detached {
			continue
		}
		g.page.Remove(g.widget)
		g.detached = true
	}
	if !u.listDrained() {
		u.listCloseHold = true
		u.app.Hold()
		u.scheduleListBatch()
	}
}

// listDrained is the release condition: slices are authoritative, the counter
// is only checked by tests so that a stale count cannot hang Hold forever.
func (u *ui) listDrained() bool {
	return u.listBuild == nil && len(u.retiredRows) == 0 && len(u.retiredGroups) == 0
}
