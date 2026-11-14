//go:build linux && gtk_e2e

package main

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"github.com/mensylisir/appclone/core"
)

// Run the same binary fixture before/after in separate processes on one host.
// Measures SetText -> applied filter (not compositor frame presentation), and
// Go allocation including the GTK main loop. No real applications are started.
func TestGtkSearchPerformance(t *testing.T) {
	if os.Getenv("APPCLONE_SEARCH_PERF") != "1" {
		t.Skip("set APPCLONE_SEARCH_PERF=1 for search measurements")
	}
	env, _ := gtkTestEnv(t, "search-perf")
	app := adw.NewApplication("io.github.mensylisir.Appclone.SearchPerf", gio.GApplicationNonUniqueValue)
	defer app.Unref()
	u := &ui{app: app, env: env, lang: "en"}
	queries := []string{"app", "a", "ap", "app", "a", "ap", "app", "a", "ap", "app", "a", "ap"}
	var samples []float64
	var before, after runtime.MemStats
	var runErr error
	var sent time.Time
	deadline := time.Now().Add(25 * time.Second)
	started, firstGen := false, 0
	var tick glib.SourceFunc
	tick = func(uintptr) bool {
		if time.Now().After(deadline) {
			runErr = fmt.Errorf("search did not settle: %d samples", len(samples))
			app.Quit()
			return false
		}
		if !u.loaded || !u.envChecked || !u.win.GetMapped() {
			return true
		}
		if !started {
			u.apps = nil
			for i := 0; i < 200; i++ {
				icon := ""
				if os.Getenv("APPCLONE_SEARCH_ICONS") == "1" {
					icon = []string{"folder", "text-x-generic", "image-x-generic"}[i%3]
				}
				u.apps = append(u.apps, core.App{Key: fmt.Sprintf("app%03d", i), Name: fmt.Sprintf("Application %03d", i), Icon: icon})
			}
			u.renderList()
			firstGen = u.listGen
			runtime.ReadMemStats(&before)
			started = true
			sent = time.Now()
			u.search.SetText(queries[0])
			return true
		}
		if u.query != queries[len(samples)] {
			return true
		}
		samples = append(samples, float64(time.Since(sent).Microseconds())/1000)
		if len(samples) == len(queries) {
			runtime.ReadMemStats(&after)
			app.Quit()
			return false
		}
		sent = time.Now()
		u.search.SetText(queries[len(samples)])
		return true
	}
	activate := func(_ gio.Application) {
		u.buildWindow()
		u.win.Present()
		glib.TimeoutAdd(5, &tick, 0)
	}
	app.ConnectActivate(&activate)
	u.hold(&activate, &tick)
	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK exited %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(samples) != len(queries) {
		t.Fatalf("missing measurements: %v", samples)
	}
	slices.Sort(samples)
	t.Logf("search: edits=%d median=%.2fms p95=%.2fms rebuilds=%d Go-bytes/edit=%d",
		len(samples), (samples[5]+samples[6])/2, samples[11], u.listGen-firstGen,
		(after.TotalAlloc-before.TotalAlloc)/uint64(len(samples)))
}
