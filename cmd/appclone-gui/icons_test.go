//go:build linux && gtk_e2e

package main

// 图标读取/解码边界与回调槽回归。默认门禁不编译（puregotk 在 init 阶段
// dlopen GTK），由 design/guitest-gtk.sh 显式编入。

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gdkpixbuf"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"
	"github.com/mensylisir/appclone/core"
)

// runGtkBody boots the product window against an isolated fake HOME and runs
// body on the GTK main loop. body returns done=false to be called again on the
// next tick, which lets async list/probe work make progress.
func runGtkBody(t *testing.T, name string, body func(u *ui) (bool, error)) {
	t.Helper()
	env, _ := gtkTestEnv(t, name)
	app := adw.NewApplication("io.github.mensylisir.Appclone."+strings.ReplaceAll(name, "-", ""),
		gio.GApplicationNonUniqueValue)
	defer app.Unref()
	u := newUI(app, env)
	var runErr error
	deadline := time.Now().Add(120 * time.Second)
	var tick glib.SourceFunc
	tick = func(uintptr) bool {
		if runErr != nil {
			return false
		}
		if time.Now().After(deadline) {
			runErr = fmt.Errorf("%s timed out", name)
			app.Quit()
			return false
		}
		if !u.loaded || !u.envChecked || !u.win.GetMapped() {
			return true
		}
		done, err := body(u)
		if err != nil {
			runErr = err
			app.Quit()
			return false
		}
		if done {
			app.Quit()
			return false
		}
		return true
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
}

func iconDims(img *gtk.Image) (int32, int32) {
	p := img.GetPaintable()
	if p == nil {
		return 0, 0
	}
	// GetPaintable hands back a reference even though the C docs say borrowed.
	defer gobject.ObjectNewFromInternalPtr(p.GoPointer()).Unref()
	return p.GetIntrinsicWidth(), p.GetIntrinsicHeight()
}

func writePNG(t *testing.T, path string, w, h int, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func filled(c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func svgFormatAvailable(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "probe.svg")
	if err := os.WriteFile(p, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"/>`), 0o600); err != nil {
		t.Fatal(err)
	}
	var w, h int32
	return gdkpixbuf.PixbufGetFileInfo(p, &w, &h) != nil
}

// TestGtkIconBounds covers every rejection path plus the two decode timings:
// PNG emits size-prepared during Write; the SVG backend may only do so inside
// Close, so the Close-timing half is exercised only when an SVG loader is
// registered — a missing loader is skipped explicitly rather than turning the
// case green through the fallback path.
func TestGtkIconBounds(t *testing.T) {
	runGtkBody(t, "icon-bounds", func(u *ui) (bool, error) {
		dir := t.TempDir()
		png64 := filepath.Join(dir, "a.png")
		writePNG(t, png64, 64, 64, color.RGBA{R: 200, A: 255})
		pngWide := filepath.Join(dir, "wide.png")
		writePNG(t, pngWide, 128, 64, color.RGBA{G: 200, A: 255})

		img, ok := u.iconImage(png64)
		if !ok {
			return false, fmt.Errorf("valid PNG fell back to avatar")
		}
		w, h := iconDims(img)
		img.Unref()
		if w != 64 || h != 64 {
			return false, fmt.Errorf("64x64 PNG decoded to %dx%d", w, h)
		}
		img, ok = u.iconImage(pngWide)
		if !ok {
			return false, fmt.Errorf("valid wide PNG fell back to avatar")
		}
		w, h = iconDims(img)
		img.Unref()
		if w != 64 || h != 32 {
			return false, fmt.Errorf("128x64 PNG decoded to %dx%d, want 64x32", w, h)
		}
		// User icon paths can contain spaces and CJK; keep the string path end to
		// end instead of assuming an ASCII temp directory.
		cjk := filepath.Join(dir, "应用 图标.png")
		writePNG(t, cjk, 64, 64, color.RGBA{B: 200, A: 255})
		img, ok = u.iconImage(cjk)
		if !ok {
			return false, fmt.Errorf("PNG with spaces/CJK in the path fell back to avatar")
		}
		w, h = iconDims(img)
		img.Unref()
		if w != 64 || h != 64 {
			return false, fmt.Errorf("CJK-path PNG decoded to %dx%d", w, h)
		}

		corrupt := filepath.Join(dir, "corrupt.png")
		if err := os.WriteFile(corrupt, []byte("not an image at all"), 0o600); err != nil {
			return false, err
		}
		if img, ok := u.iconImage(corrupt); ok {
			img.Unref()
			return false, fmt.Errorf("corrupt PNG decoded")
		}

		oversize := filepath.Join(dir, "oversize.png")
		if err := os.WriteFile(oversize, make([]byte, maxIconFileBytes+1), 0o600); err != nil {
			return false, err
		}
		if img, ok := u.iconImage(oversize); ok {
			img.Unref()
			return false, fmt.Errorf(">4MiB icon decoded")
		}

		// 4096² is rejected from the header alone: no pixel buffer is allocated.
		huge := filepath.Join(dir, "huge.png")
		writePNG(t, huge, 4096, 4096, color.RGBA{B: 200, A: 255})
		if info, err := os.Stat(huge); err != nil {
			return false, err
		} else if info.Size() > maxIconFileBytes {
			return false, fmt.Errorf("4096² fixture unexpectedly exceeds the byte cap")
		}
		if img, ok := u.iconImage(huge); ok {
			img.Unref()
			return false, fmt.Errorf("4096² icon decoded")
		}

		if img, ok := u.iconImage("/dev/null"); ok {
			img.Unref()
			return false, fmt.Errorf("device node decoded")
		}

		// FIFO and symlink→FIFO must return immediately, not park the main loop.
		fifo := filepath.Join(dir, "pipe")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			return false, err
		}
		link := filepath.Join(dir, "pipe-link")
		if err := os.Symlink(fifo, link); err != nil {
			return false, err
		}
		for _, p := range []string{fifo, link} {
			if img, ok := u.iconImage(p); ok {
				img.Unref()
				return false, fmt.Errorf("%s decoded as a regular icon", p)
			}
			// Helper level: bypass the Stat fast path and prove O_NONBLOCK+fstat.
			info, err := os.Stat(p)
			if err != nil {
				return false, err
			}
			key, ok := iconKeyOf(p, info)
			if !ok {
				return false, fmt.Errorf("no stat key for %s", p)
			}
			done := make(chan bool, 1)
			go func(path string) {
				_, ok := u.loadIconFile(path, key)
				done <- ok
			}(p)
			select {
			case ok := <-done:
				if ok {
					return false, fmt.Errorf("%s decoded through the helper", p)
				}
			case <-time.After(2 * time.Second):
				return false, fmt.Errorf("%s blocked the helper", p)
			}
		}

		if svgFormatAvailable(t, dir) {
			svg64 := filepath.Join(dir, "a.svg")
			if err := os.WriteFile(svg64, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" fill="#3584e4"/></svg>`), 0o600); err != nil {
				return false, err
			}
			img, ok := u.iconImage(svg64)
			if !ok {
				return false, fmt.Errorf("valid SVG fell back to avatar")
			}
			w, h := iconDims(img)
			img.Unref()
			if w != 64 || h != 64 {
				return false, fmt.Errorf("SVG decoded to %dx%d", w, h)
			}
			// No intrinsic size: the backend may pick its own default (300×300
			// here) but the output still has to fit 64.
			svgFree := filepath.Join(dir, "free.svg")
			if err := os.WriteFile(svgFree, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="10" height="10" fill="#e01b24"/></svg>`), 0o600); err != nil {
				return false, err
			}
			img, ok = u.iconImage(svgFree)
			if !ok {
				return false, fmt.Errorf("size-less SVG fell back to avatar")
			}
			w, h = iconDims(img)
			img.Unref()
			if w < 1 || h < 1 || w > 64 || h > 64 {
				return false, fmt.Errorf("size-less SVG decoded to %dx%d", w, h)
			}
		} else {
			t.Log("SKIP SVG cases: no SVG loader registered (PixbufGetFileInfo returned nil)")
		}

		// A cached file that disappears must fall back, not reuse the old texture.
		gone := filepath.Join(dir, "gone.png")
		writePNG(t, gone, 32, 32, color.RGBA{R: 1, G: 2, B: 3, A: 255})
		if img, ok := u.iconImage(gone); ok {
			img.Unref()
		} else {
			return false, fmt.Errorf("valid 32x32 PNG fell back")
		}
		if err := os.Remove(gone); err != nil {
			return false, err
		}
		if img, ok := u.iconImage(gone); ok {
			img.Unref()
			return false, fmt.Errorf("deleted icon reused a cached texture")
		}

		// Directory-entry replacement with the same size and mtime: the inode is
		// different, so the old cache key must not be reused. Both fixtures are
		// padded to one fixed byte length (trailing bytes after IEND are ignored).
		const replaceSize = 4096
		pngBytes := func(c color.Color) []byte {
			var b bytes.Buffer
			if err := png.Encode(&b, filled(c)); err != nil {
				t.Fatal(err)
			}
			if b.Len() > replaceSize {
				t.Fatalf("replacement fixture %d > %d", b.Len(), replaceSize)
			}
			return append(b.Bytes(), make([]byte, replaceSize-b.Len())...)
		}
		oldPath := filepath.Join(dir, "replace.png")
		if err := os.WriteFile(oldPath, pngBytes(color.RGBA{R: 255, A: 255}), 0o600); err != nil {
			return false, err
		}
		oldInfo, err := os.Stat(oldPath)
		if err != nil {
			return false, err
		}
		oldKey, ok := iconKeyOf(oldPath, oldInfo)
		if !ok {
			return false, fmt.Errorf("no cache key for replacement fixture")
		}
		if img, ok := u.iconImage(oldPath); ok {
			img.Unref()
		} else {
			return false, fmt.Errorf("replacement fixture did not decode")
		}
		tmp := filepath.Join(dir, "replacement.tmp")
		if err := os.WriteFile(tmp, pngBytes(color.RGBA{B: 255, A: 255}), 0o600); err != nil {
			return false, err
		}
		if err := os.Chtimes(tmp, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
			return false, err
		}
		if err := os.Rename(tmp, oldPath); err != nil {
			return false, err
		}
		if after, err := os.Stat(oldPath); err != nil {
			return false, err
		} else if after.Size() != oldInfo.Size() || !after.ModTime().Equal(oldInfo.ModTime()) {
			return false, fmt.Errorf("replacement changed size/mtime unexpectedly")
		}
		if img, ok := u.iconImage(oldPath); !ok {
			return false, fmt.Errorf("replaced icon fell back to avatar")
		} else {
			img.Unref()
		}
		newInfo, err := os.Stat(oldPath)
		if err != nil {
			return false, err
		}
		newKey, ok := iconKeyOf(oldPath, newInfo)
		if !ok {
			return false, fmt.Errorf("no cache key after replacement")
		}
		if newKey == oldKey {
			return false, fmt.Errorf("replacement kept the old inode key")
		}
		if _, hit := u.iconCache[newKey]; !hit {
			return false, fmt.Errorf("replacement was not re-decoded under its new inode key")
		}
		return true, nil
	})
}

// TestGtkIconCallbackSlots decodes 2000 unique paths: a per-decode closure
// would exhaust purego's permanent 2000 callback slots and panic. GC is pinned
// so a collected closure cannot recycle the address and mask the bug.
func TestGtkIconCallbackSlots(t *testing.T) {
	runGtkBody(t, "icon-slots", func(u *ui) (bool, error) {
		dir := t.TempDir()
		var buf bytes.Buffer
		if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
			return false, err
		}
		var before uintptr
		old := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(old)
		for i := 0; i < 2000; i++ {
			p := filepath.Join(dir, fmt.Sprintf("icon%04d.png", i))
			if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
				return false, err
			}
			img, ok := u.iconImage(p)
			if !ok {
				return false, fmt.Errorf("unique PNG %d failed to decode", i)
			}
			img.Unref()
			if i == 0 {
				before, ok = glib.GetCallback(uintptr(unsafe.Pointer(&u.cb.sizePrepFn)))
				if !ok || before == 0 {
					return false, fmt.Errorf("size-prepare callback was never registered")
				}
			}
		}
		after, ok := glib.GetCallback(uintptr(unsafe.Pointer(&u.cb.sizePrepFn)))
		if !ok || after != before {
			return false, fmt.Errorf("size-prepare callback address changed: %x -> %x", before, after)
		}
		u.cb.mu.Lock()
		pending := len(u.cb.loaders)
		u.cb.mu.Unlock()
		if pending != 0 {
			return false, fmt.Errorf("%d loader states leaked", pending)
		}
		if len(u.iconCache) > iconCacheLimit {
			return false, fmt.Errorf("icon cache grew to %d", len(u.iconCache))
		}
		return true, nil
	})
}

// TestGtkDistinctIconBatches is manual/perf only (skipped by guitest-gtk): 200
// rows with unique cold SVGs must keep every batch inside the 16ms hard gate and
// the 4-row allowance.
func TestGtkDistinctIconBatches(t *testing.T) {
	if os.Getenv("APPCLONE_SEARCH_PERF") != "1" {
		t.Skip("set APPCLONE_SEARCH_PERF=1 for cold-icon batch measurements")
	}
	dir := t.TempDir()
	if !svgFormatAvailable(t, dir) {
		t.Fatal("no SVG loader registered; cannot measure cold file icons")
	}
	var worstBatch time.Duration
	var worstRows string
	var batches []time.Duration
	over16 := 0
	batchNo := 0
	started := false
	runGtkBody(t, "distinct-icons", func(u *ui) (bool, error) {
		// A poll snapshot landing between injection and render could replace the
		// fixture and leave only a couple of trivial batches, passing the p95 gate
		// on nothing. Pause polling and require the full fixture to have rendered.
		u.pollPaused = true
		u.listObserve = func(m listMeasurement) {
			if m.Prepare {
				return
			}
			batchNo++
			if m.Created+m.Retired > 4 {
				worstRows = fmt.Sprintf("batch created %d retired %d", m.Created, m.Retired)
			}
			batches = append(batches, m.Duration)
			if m.Duration > 16*time.Millisecond {
				over16++
			}
			if m.Duration > worstBatch {
				worstBatch = m.Duration
			}
			if m.Duration > 8*time.Millisecond {
				t.Logf("slow distinct batch #%d size/kind=%s created=%d retired=%d duration=%.2fms",
					batchNo, "200/distinct", m.Created, m.Retired, perfMS(m.Duration))
			}
		}
		if started {
			for !u.listSettled() {
				return false, nil
			}
			slices.Sort(batches)
			if len(u.listRows) != 200 || len(batches) < 50 {
				return false, fmt.Errorf("fixture did not render fully: %d rows in %d batches", len(u.listRows), len(batches))
			}
			p95 := batches[(95*len(batches)+99)/100-1]
			t.Logf("distinct-batch-p95=%.2fms distinct-batch-max=%.2fms over16=%d", perfMS(p95), perfMS(worstBatch), over16)
			if worstRows != "" {
				return false, fmt.Errorf("batch allowance exceeded: %s", worstRows)
			}
			// Wall-clock p95 is the gate: a single batch can lose the CPU to the
			// compositor or the kernel even with a fixed fixture (docs §12.5 already
			// refuses a real-time promise). Every >8ms batch above is logged.
			if p95 > 16*time.Millisecond {
				return false, fmt.Errorf("distinct-batch-p95 %.2fms > 16ms (max %.2fms)", perfMS(p95), perfMS(worstBatch))
			}
			return true, nil
		}
		started = true
		u.apps = make([]core.App, 0, 200)
		for i := 0; i < 200; i++ {
			p := filepath.Join(dir, fmt.Sprintf("app%03d.svg", i))
			svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" fill="rgb(%d,%d,%d)"/></svg>`, i%256, (i*7)%256, (i*13)%256)
			if err := os.WriteFile(p, []byte(svg), 0o600); err != nil {
				return false, err
			}
			u.apps = append(u.apps, core.App{Key: fmt.Sprintf("distinct%03d", i),
				Name: fmt.Sprintf("Distinct %03d", i), Icon: p})
		}
		u.expanded = true
		u.stack.SetVisibleChildName("all")
		u.renderList()
		return false, nil
	})
}

// Pure helpers: no loader involved, so they pin 0/negative/4096² and the
// sized==false branch regardless of backend behavior.
func TestIconPureHelpers(t *testing.T) {
	cases := []struct {
		w, h, sw, sh int32
	}{
		{64, 64, 64, 64}, {128, 64, 64, 32}, {1, 1, 1, 1}, {4096, 4096, 0, 0},
		{0, 0, 64, 64}, {-5, 20, 64, 64}, {1025, 1, 0, 0}, {2000, 600, 0, 0},
	}
	for _, c := range cases {
		kind, sw, sh := iconTargetSize(c.w, c.h)
		switch {
		case c.sw == 0 && kind != iconSizeReject:
			t.Errorf("%dx%d: kind=%d, want reject", c.w, c.h, kind)
		case c.sw != 0 && (kind != iconSizeScale || sw != c.sw || sh != c.sh):
			t.Errorf("%dx%d: got kind=%d %dx%d, want scale %dx%d", c.w, c.h, kind, sw, sh, c.sw, c.sh)
		}
	}
	if iconFinalOK(false, false, 64, 64) {
		t.Error("missing size-prepared must be rejected")
	}
	if iconFinalOK(true, true, 64, 64) {
		t.Error("oversize flag must be rejected")
	}
	if iconFinalOK(true, false, 0, 64) || iconFinalOK(true, false, 65, 64) {
		t.Error("final pixels outside 1..64 must be rejected")
	}
	if !iconFinalOK(true, false, 64, 64) {
		t.Error("valid final size rejected")
	}
}
