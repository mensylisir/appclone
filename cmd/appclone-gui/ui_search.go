package main

import (
	"strings"

	"github.com/mensylisir/appclone/core"
)

// appList is the visible projection of a single loaded snapshot. Rendering and
// search comparison use the same limits and tail rows; no disk data is cached.
type appList struct {
	on, off []core.App
	total   int
	tail    string // more | limited | no_match | all_clones | empty
}

func projectAppList(apps []core.App, query string, expanded bool) appList {
	var view appList
	limit := len(apps)
	if query != "" {
		limit = min(limit, searchLimit)
	} else if !expanded {
		limit = min(limit, listLimit)
	}
	for _, a := range apps {
		if a.InstanceN > 0 {
			view.on = append(view.on, a)
		} else if query == "" || strings.Contains(strings.ToLower(a.Name), query) ||
			strings.Contains(strings.ToLower(a.Key), query) {
			view.total++
			if len(view.off) < limit {
				if view.off == nil {
					view.off = make([]core.App, 0, limit)
				}
				view.off = append(view.off, a)
			}
		}
	}
	switch {
	case view.total == 0 && query != "":
		view.tail = "no_match"
	case view.total == 0:
		view.tail = "all_clones"
	case query == "" && !expanded && view.total > listLimit:
		view.off, view.tail = view.off[:listLimit], "more"
	case query != "" && view.total > searchLimit:
		view.off, view.tail = view.off[:searchLimit], "limited"
	}
	return view
}

func sameSearchResults(a, b appList) bool {
	if a.tail != b.tail || a.total != b.total || len(a.off) != len(b.off) {
		return false
	}
	// No-match text includes the query, even when both result sets are empty.
	if b.tail == "no_match" {
		return false
	}
	for i := range a.off {
		if a.off[i].Key != b.off[i].Key {
			return false
		}
	}
	return true
}

// GTK's search-changed signal already debounces typing. Only a changed visible
// result needs new widgets; otherwise keep focus, scroll and signal handlers.
func (u *ui) applySearch() {
	q := strings.ToLower(strings.TrimSpace(u.search.GetText()))
	if q == u.query {
		return
	}
	previous := u.query
	u.query = q
	if !u.loaded {
		return // the first scan renders with the latest query when it arrives
	}
	if !sameSearchResults(projectAppList(u.apps, previous, u.expanded), projectAppList(u.apps, q, u.expanded)) {
		u.renderList()
	}
}
