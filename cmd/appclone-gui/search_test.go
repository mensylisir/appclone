//go:build linux && gtk_e2e

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"
	"github.com/mensylisir/appclone/core"
)

func TestGtkSearchResults(t *testing.T) {
	env, _ := gtkTestEnv(t, "search")
	app := adw.NewApplication("io.github.mensylisir.Appclone.Search", gio.GApplicationNonUniqueValue)
	defer app.Unref()
	u := &ui{app: app, env: env, lang: "en"}
	cases := []struct {
		query string
		rows  int
		text  string
		keep  bool
	}{
		{"app", 101, "200", false},
		{"a", 101, "200", true},
		{"missing-1", 1, "missing-1", false},
		{"  MISSING-2  ", 1, "missing-2", false},
		{"APP012", 1, "Application 012", false},
		{"  APP012  ", 1, "Application 012", true},
		{"", 31, "200", false},
		{"app", 101, "200", false},
		{"", 200, "Application 199", false},
	}
	step, gen := -1, 0
	var sent time.Time
	var runErr error
	deadline := time.Now().Add(20 * time.Second)
	fail := func(format string, args ...any) { runErr = fmt.Errorf(format, args...); app.Quit() }
	var tick glib.SourceFunc
	tick = func(uintptr) bool {
		if time.Now().After(deadline) {
			fail("search timed out at %d", step)
			return false
		}
		if !u.loaded || !u.envChecked || !u.win.GetMapped() {
			return true
		}
		if step == -1 {
			u.apps = nil
			for i := 0; i < 200; i++ {
				u.apps = append(u.apps, core.App{Key: fmt.Sprintf("app%03d", i), Name: fmt.Sprintf("Application %03d", i), Icon: "folder"})
			}
			u.renderList()
			step = 0
		} else {
			c := cases[step]
			if time.Since(sent) < 200*time.Millisecond || u.query != strings.ToLower(strings.TrimSpace(c.query)) {
				return true
			}
			titles := listRowTitles(u.offPage)
			if len(titles) != c.rows || !strings.Contains(strings.Join(titles, "\n"), c.text) {
				fail("query %q: got %d rows, expected %d and %q", c.query, len(titles), c.rows, c.text)
				return false
			}
			if c.keep && u.listGen != gen {
				fail("unchanged results rebuilt for %q", c.query)
				return false
			}
			if !c.keep && u.listGen != gen+1 {
				fail("query %q rebuilt %d times", c.query, u.listGen-gen)
				return false
			}
			if step == 6 {
				row := findListRowByTitle(u.offPage, u.tr("ui.row.show_all", map[string]any{"n": 200}))
				if row == nil {
					fail("show-all action missing")
					return false
				}
				gobject.SignalEmitByName(&row.Object, "activated")
				if !u.expanded || len(listRowTitles(u.offPage)) != 200 {
					fail("show-all action did not expand")
					return false
				}
			}
			step++
			if step == len(cases) {
				app.Quit()
				return false
			}
		}
		gen, sent = u.listGen, time.Now()
		u.search.SetText(cases[step].query)
		return true
	}
	activate := func(_ gio.Application) {
		u.buildWindow()
		u.win.Present()
		glib.TimeoutAdd(10, &tick, 0)
	}
	app.ConnectActivate(&activate)
	u.hold(&activate, &tick)
	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK exited %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if step != len(cases) {
		t.Fatalf("incomplete search test: %d", step)
	}
}

func TestSearchProjection(t *testing.T) {
	apps := []core.App{{Key: "clone", Name: "Cloned", InstanceN: 1}}
	for i := 0; i < 201; i++ {
		apps = append(apps, core.App{Key: fmt.Sprintf("app%03d", i), Name: "应用"})
	}
	all := projectAppList(apps, "", false)
	if len(all.on) != 1 || len(all.off) != 30 || all.total != 201 || all.tail != "more" {
		t.Fatal("collapsed projection")
	}
	if len(projectAppList(apps, "", true).off) != 201 {
		t.Fatal("expanded projection")
	}
	a, b := projectAppList(apps, "app", false), projectAppList(apps, "应用", false)
	if !sameSearchResults(a, b) || len(a.off) != 100 || a.tail != "limited" || len(a.on) != 1 {
		t.Fatal("search projection/clone visibility")
	}
	// Same first 100 rows, changed overflow total: the tail must be refreshed.
	if sameSearchResults(a, projectAppList(apps[:201], "app", false)) {
		t.Fatal("stale overflow count")
	}
	if sameSearchResults(projectAppList(apps, "missing-a", false), projectAppList(apps, "missing-b", false)) {
		t.Fatal("stale empty-query text")
	}
}

// Host integration opt-in: only desktop metadata/icons, all tool state remains
// in a fake HOME. The named installed applications must resolve real icons.
func TestGtkInstalledAppIcons(t *testing.T) {
	names := os.Getenv("APPCLONE_CHECK_ICONS")
	if names == "" {
		t.Skip("set APPCLONE_CHECK_ICONS to comma-separated desktop IDs")
	}
	env, _ := gtkTestEnv(t, "installed-icons")
	env.AppDirList = []string{"/usr/share/applications"}
	gtk.Init()
	u := &ui{env: env, lang: "en"}
	apps, scanErrs := env.ListApps(context.Background())
	if len(scanErrs) != 0 {
		t.Logf("desktop scan warnings: %d", len(scanErrs))
	}
	for _, key := range strings.Split(names, ",") {
		found := false
		for _, a := range apps {
			if !strings.EqualFold(a.Key, key) {
				continue
			}
			found = true
			w, fallback := u.appIcon(a.Icon, a.Name, 32)
			if w == nil || fallback {
				t.Errorf("%s icon %q unexpectedly fell back", key, a.Icon)
			}
			if w != nil {
				w.Unref()
			}
			t.Logf("%s: icon=%s fallback=%t", key, a.Icon, fallback)
		}
		if !found {
			t.Errorf("installed desktop %q not found", key)
		}
	}
}
