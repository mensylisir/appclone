//go:build linux && gtk_e2e

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gdkpixbuf"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"
	"github.com/mensylisir/appclone/core"
)

// processStarted is the test binary init time (the closest test-visible proxy
// for process start) used only by the startup diagnostic.
var processStarted = time.Now()

// perfWarmupRan records that the throwaway first window already existed in this
// process. Without it the very first GL window costs ~430ms on hosts with a
// broken GL context, so the 300ms startup gate is only meaningful after warm-up.
var perfWarmupRan bool

type listPerfSample struct {
	size                                int
	kind                                string
	filter, mapped, complete, reclaimed time.Duration
	gap, batch, prepare                 time.Duration
}

// TestGtkPerfWarmup boots one throwaway window in the same process before the
// measurement. On hosts whose GL context is broken (this machine logs
// gdk_gl_context_make_current() failed), the very first window costs ~430ms
// regardless of list work; without warm-up the startup valve would measure GL
// cold-init instead of the app/list path. The cold number is still printed as
// the process-start diagnostic in TestGtkSearchPerformance.
func TestGtkPerfWarmup(t *testing.T) {
	if os.Getenv("APPCLONE_SEARCH_PERF") != "1" {
		t.Skip("set APPCLONE_SEARCH_PERF=1 for search measurements")
	}
	runGtkBody(t, "perf-warmup", func(*ui) (bool, error) { return true, nil })
	perfWarmupRan = true
}

// Each process measures both sizes with real theme/SVG/PNG icons. Mapped means
// GTK widget mapping, not compositor presentation or physical input latency.
func TestGtkSearchPerformance(t *testing.T) {
	if os.Getenv("APPCLONE_SEARCH_PERF") != "1" {
		t.Skip("set APPCLONE_SEARCH_PERF=1 for search measurements")
	}
	// Gate on this test's own start: in the combined -run the process may already
	// have executed the distinct-icon case, so process start is not a valid
	// startup baseline. The process-wide number is still printed as a diagnostic.
	testStart := time.Now()
	env, _ := gtkTestEnv(t, "search-perf")
	iconsDir := t.TempDir()
	svg, bitmap := filepath.Join(iconsDir, "sample.svg"), filepath.Join(iconsDir, "sample.png")
	if err := os.WriteFile(svg, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" fill="#3584e4"/></svg>`), 0600); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bitmap, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	icons := []string{"folder", svg, bitmap}
	app := adw.NewApplication("io.github.mensylisir.Appclone.SearchPerf", gio.GApplicationNonUniqueValue)
	defer app.Unref()
	u := newUI(app, env)
	queries := []string{"", "application 00", "APPLICATION 00", "application 01", "", "application 00", "APPLICATION 00", "application 01", "", "application 00", "APPLICATION 00", "application 01", "", "application 00", "APPLICATION 00", "application 01"}
	sizes := []int{200, 1000}
	sizeIndex, step := 0, 0
	var sample *listPerfSample
	var samples []listPerfSample
	var sent, lastTick time.Time
	var runErr error
	var before, after runtime.MemStats
	wantQuery, wantGen, wantRows := "", 0, 0
	startupLogged := false
	var buildDur, maxPaint, fileInfoDur time.Duration
	var lastFrame time.Time
	var frameCb gtk.TickCallback
	var destroyCb glib.DestroyNotify
	iconsChecked := false
	deadline := time.Now().Add(120 * time.Second)
	u.listObserve = func(m listMeasurement) {
		if sample == nil {
			return
		}
		if m.Prepare {
			sample.prepare = max(sample.prepare, m.Duration)
		} else {
			sample.batch = max(sample.batch, m.Duration)
			if m.Duration > 8*time.Millisecond {
				t.Logf("slow-batch size=%d kind=%s Created=%d Retired=%d duration=%.2fms", sample.size, sample.kind, m.Created, m.Retired, perfMS(m.Duration))
			}
		}
		if m.Created > 4 || m.Created+m.Retired > 4 {
			runErr = fmt.Errorf("batch exceeded row allowance: %+v", m)
		}
	}
	var tick glib.SourceFunc
	tick = func(uintptr) bool {
		if runErr != nil || time.Now().After(deadline) {
			if runErr == nil {
				runErr = fmt.Errorf("performance timed out size=%d step=%d", sizeIndex, step)
			}
			app.Quit()
			return false
		}
		if !u.loaded || !u.envChecked || !u.win.GetMapped() {
			return true
		}
		// Measurements own u.apps/u.query; a product poll snapshot must not collide.
		u.pollPaused = true
		now := time.Now()
		if !startupLogged {
			startupLogged = true
			startup := now.Sub(testStart)
			t.Logf("startup-max-gap=%.2fms process-start=%.2fms buildWindow=%.2fms (loader warm-up is one-off, outside list gates)",
				perfMS(startup), perfMS(now.Sub(processStarted)), perfMS(buildDur))
			if startup > 300*time.Millisecond {
				if perfWarmupRan {
					runErr = fmt.Errorf("test start to first frame exceeded 300ms: %s", startup)
					return true
				}
				// A standalone run has no warm-up window; this number is GL cold
				// start, not the app/list path. Print it, but do not gate on it.
				t.Logf("SKIP startup gate: no TestGtkPerfWarmup in this process (cold start %.2fms)", perfMS(startup))
			}
			lastTick = now
		}
		if !iconsChecked {
			iconsChecked = true
			if img, ok := u.iconImage(bitmap); !ok {
				runErr = fmt.Errorf("PNG icon failed to decode through iconImage")
				return true
			} else {
				img.Unref()
			}
			infoStart := time.Now()
			var fw, fh int32
			for i := 0; i < 200; i++ {
				gdkpixbuf.PixbufGetFileInfo(filepath.Join(iconsDir, "sample.png"), &fw, &fh)
			}
			fileInfoDur = time.Since(infoStart)
			// Only skip the SVG assertion when the format is not registered at
			// all; a registered backend that fails to decode is a hard failure.
			var sw, sh int32
			if gdkpixbuf.PixbufGetFileInfo(svg, &sw, &sh) != nil {
				if img, ok := u.iconImage(svg); !ok {
					runErr = fmt.Errorf("SVG loader present but iconImage rejected the SVG")
					return true
				} else {
					img.Unref()
				}
			} else {
				t.Log("SKIP SVG decode assertion: no SVG loader registered")
			}
		}
		if sample != nil {
			if !lastTick.IsZero() {
				sample.gap = max(sample.gap, now.Sub(lastTick))
			}
			lastTick = now
			if u.query != wantQuery || u.listGen != wantGen {
				return true
			}
			if sample.filter == 0 {
				sample.filter = now.Sub(sent)
			}
			if sample.mapped == 0 {
				for _, r := range u.listRows {
					if r.widget.GetMapped() {
						sample.mapped = time.Since(sent)
						break
					}
				}
			}
			if u.listBuild == nil && sample.complete == 0 {
				if len(u.listRows) != wantRows {
					runErr = fmt.Errorf("rendered %d rows, wanted %d", len(u.listRows), wantRows)
					return true
				}
				sample.complete = time.Since(sent)
			}
			if !u.listSettled() || sample.mapped == 0 {
				return true
			}
			sample.reclaimed = time.Since(sent)
			samples = append(samples, *sample)
			t.Logf("size=%d kind=%s filter=%.2fms mapped=%.2fms complete=%.2fms reclaimed=%.2fms max-gap=%.2fms max-batch=%.2fms prepare=%.2fms", sample.size, sample.kind,
				perfMS(sample.filter), perfMS(sample.mapped), perfMS(sample.complete), perfMS(sample.reclaimed), perfMS(sample.gap), perfMS(sample.batch), perfMS(sample.prepare))
			sample = nil
			step++
			if step == len(queries) {
				step = 0
				sizeIndex++
			}
			if sizeIndex == len(sizes) {
				runtime.ReadMemStats(&after)
				app.Quit()
				return false
			}
		}
		if !u.listSettled() {
			return true
		}
		fresh := step == 0
		if fresh {
			if sizeIndex == 0 {
				runtime.ReadMemStats(&before)
			}
			u.apps = make([]core.App, sizes[sizeIndex])
			for i := range u.apps {
				u.apps[i] = core.App{Key: fmt.Sprintf("opaque%04d", i), Name: fmt.Sprintf("Application %04d", i), Icon: icons[i%len(icons)]}
			}
			u.expanded = true
			u.stack.SetVisibleChildName("all")
		}
		q := queries[step]
		wantQuery = strings.ToLower(q)
		view := projectAppList(u.apps, wantQuery, u.expanded)
		wantRows = len(view.off)
		if view.tail != "" {
			wantRows++
		}
		wantGen = u.listGen
		kind := "search"
		if fresh || !sameSearchResults(projectAppList(u.apps, u.query, u.expanded), view) {
			wantGen++
		} else {
			kind = "kept"
		}
		if q == "" {
			kind = "full"
		}
		sample = &listPerfSample{size: sizes[sizeIndex], kind: kind}
		sent, lastTick = time.Now(), time.Now()
		if fresh {
			u.query = wantQuery
			u.search.SetText(q)
			u.renderList()
		} else {
			u.search.SetText(q)
		}
		return true
	}
	activate := func(_ gio.Application) {
		buildStart := time.Now()
		u.buildWindow()
		buildDur = time.Since(buildStart)
		frameCb = func(uintptr, uintptr, uintptr) bool {
			now := time.Now()
			if !lastFrame.IsZero() {
				maxPaint = max(maxPaint, now.Sub(lastFrame))
			}
			lastFrame = now
			return true
		}
		destroyCb = func(uintptr) {}
		u.hold(&frameCb, &destroyCb)
		u.win.AddTickCallback(&frameCb, 0, &destroyCb)
		glib.TimeoutAdd(2, &tick, 0)
	}
	app.ConnectActivate(&activate)
	u.hold(&activate, &tick)
	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK exited %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(samples) != len(queries)*len(sizes) {
		t.Fatalf("missing measurements: %d", len(samples))
	}
	for _, size := range sizes {
		for _, kind := range []string{"full", "search", "kept"} {
			var mapped []time.Duration
			var batch, gap, prepare time.Duration
			for _, s := range samples {
				if s.size == size && s.kind == kind {
					mapped = append(mapped, s.mapped)
					batch = max(batch, s.batch)
					gap = max(gap, s.gap)
					prepare = max(prepare, s.prepare)
				}
			}
			slices.Sort(mapped)
			p95 := mapped[(95*len(mapped)+99)/100-1]
			t.Logf("summary size=%d kind=%s n=%d mapped-p95=%.2fms max-batch=%.2fms prepare=%.2fms max-gap=%.2fms", size, kind, len(mapped), perfMS(p95), perfMS(batch), perfMS(prepare), perfMS(gap))
			if kind != "kept" && (p95 > 150*time.Millisecond || batch > 16*time.Millisecond || prepare > 20*time.Millisecond) {
				t.Errorf("performance target exceeded size=%d kind=%s: mapped-p95=%s max-batch=%s prepare=%s", size, kind, p95, batch, prepare)
			}
		}
	}
	var nonFirstGap time.Duration
	for i, s := range samples {
		bound := 150 * time.Millisecond
		if i == 0 {
			bound = 300 * time.Millisecond
		} else {
			nonFirstGap = max(nonFirstGap, s.gap)
		}
		if s.gap > bound {
			t.Errorf("max-gap sample %d (%d/%s) %s > %s", i, s.size, s.kind, s.gap, bound)
		}
	}
	t.Logf("non-first-max-gap=%.2fms", perfMS(nonFirstGap))
	t.Logf("diagnostics max-paint=%.2fms file-info-200=%.2fms textures=%d rss=%s", perfMS(maxPaint), perfMS(fileInfoDur), len(u.iconCache), readHWM())
	t.Logf("Go allocation total=%d bytes for %d samples", after.TotalAlloc-before.TotalAlloc, len(samples))
}

func perfMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// readHWM reports the kernel's peak resident set for diagnostics only.
func readHWM() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	return "unknown"
}
