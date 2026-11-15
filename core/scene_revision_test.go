package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// A lifecycle fault wrapper uses only synthetic scenes; no application starts.
type sceneFaultMounter struct {
	base      OverlayMounter
	available func()
	mounted   func(string)
	mounts    int
	stops     int
}

func (m *sceneFaultMounter) Available() error {
	if m.available != nil {
		m.available()
	}
	return m.base.Available()
}
func (m *sceneFaultMounter) Mount(l, u, w, merged string) (func() error, error) {
	m.mounts++
	stop, err := m.base.Mount(l, u, w, merged)
	if err != nil {
		return nil, err
	}
	if m.mounted != nil {
		m.mounted(filepath.Dir(merged))
	}
	return func() error { m.stops++; return stop() }, nil
}

func TestProbeActiveReplacementBlocksOuterTeardown(t *testing.T) {
	e := probeEnv(t)
	var work string
	m := &sceneFaultMounter{base: &fakeMounter{}, mounted: func(path string) {
		work = path
		if err := os.Rename(filepath.Join(path, "active.json"), filepath.Join(path, "original-active")); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(path, "active.json"), "replacement")
		writeTestFile(t, filepath.Join(path, "upper", "sentinel"), "preserve")
	}}
	r := &fakeRunner{}
	_, err := e.probeSession(context.Background(), "testapp", ProbeOptions{Command: []string{"/nonexistent/scene-test"}, Mounter: m, Runner: r}, m, r)
	if err == nil || work == "" || r.calls != 0 || m.stops != 0 {
		t.Fatalf("unsafe outer teardown: %v, runner=%d stop=%d", err, r.calls, m.stops)
	}
	assertTestFile(t, filepath.Join(work, "active.json"), "replacement")
	assertTestFile(t, filepath.Join(work, "upper", "sentinel"), "preserve")
}

func TestProbeKnownMissingAndInactiveStateGuardWholeScene(t *testing.T) {
	for _, kind := range []string{"initial-missing", "stale-missing", "stale-inactive"} {
		t.Run(kind, func(t *testing.T) {
			s := markerScene(t)
			writeTestFile(t, filepath.Join(s.path, "sentinel"), "preserve")
			marker := filepath.Join(s.path, "active.json")
			if kind == "initial-missing" {
				s.activeKnown = true
			} else {
				if kind == "stale-inactive" {
					boot, err := readProbeBootID()
					if err != nil {
						t.Fatal(err)
					}
					owner, err := identifySessionProcess(os.Getpid(), 0)
					if err != nil {
						t.Fatal(err)
					}
					owner.Start++
					data, err := json.Marshal(probeActiveRecord{Version: 1, BootID: boot, Owner: owner})
					if err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, marker, string(data))
				}
				if s.probeActive() {
					t.Fatal("synthetic inactive scene blocked")
				}
			}
			if kind == "stale-inactive" {
				if err := os.Rename(marker, marker+".old"); err != nil {
					t.Fatal(err)
				}
			}
			writeTestFile(t, marker, "replacement")
			if writeProbeActive(s, false, nil) == nil {
				t.Fatal("unexpected marker overwritten")
			}
			// An observed identity failure stays rejected even if the marker disappears.
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			if s.remove(".") == nil {
				t.Fatal("identity failure forgotten during cleanup")
			}
			assertTestFile(t, filepath.Join(s.path, "sentinel"), "preserve")
		})
	}
}

func TestMissingOverlayScenePinsNearestParent(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(strconv.FormatBool(nested), func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "parent 中文")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "scene")
			if nested {
				path = filepath.Join(parent, "missing", "deeper", "scene")
			}
			s, err := openOverlayScene(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			if err := s.checkUnmounted(); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(parent, parent+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			if s.checkUnmounted() == nil {
				t.Fatal("same missing path under replacement parent accepted")
			}
		})
	}
}

func TestOverlayUpperReadsWhileSiblingMountedAndRejectsDrift(t *testing.T) {
	m := forkTableFixture(t)
	s := markerScene(t)
	upper := filepath.Join(s.path, "upper")
	writeTestFile(t, filepath.Join(upper, ".config", "App", "state"), "preserve")
	if err := s.captureUpper(); err != nil {
		t.Fatal(err)
	}
	m.points = []string{filepath.Join(s.path, "merged")}
	m.save()
	entries, present, err := s.readUpper()
	if err != nil || !present || len(entries) == 0 {
		t.Fatalf("ordinary upper unavailable while merged mounted: %v", err)
	}
	if err := os.Rename(upper, upper+".old"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(upper, "sentinel"), "replacement")
	if _, _, err := s.readUpper(); err == nil {
		t.Fatal("upper was recaptured")
	}
	m.points = nil
	m.save()
	if s.remove(".") == nil {
		t.Fatal("replacement upper deleted after read failure")
	}
	assertTestFile(t, filepath.Join(upper, "sentinel"), "replacement")
	assertTestFile(t, filepath.Join(upper+".old", ".config", "App", "state"), "preserve")
}

func TestOverlayUpperRejectsExternalLinkBeforeReadOrCleanup(t *testing.T) {
	for _, fuse := range []bool{false, true} {
		t.Run(strconv.FormatBool(fuse), func(t *testing.T) {
			m := forkTableFixture(t)
			s := markerScene(t)
			external := t.TempDir()
			writeTestFile(t, filepath.Join(external, "sentinel"), "preserve")
			if err := os.Symlink(external, filepath.Join(s.path, "upper")); err != nil {
				t.Fatal(err)
			}
			if fuse {
				m.points = []string{external}
				m.save()
			}
			if _, _, err := s.readUpper(); err == nil {
				t.Fatal("external upper link read")
			}
			if s.remove(".") == nil {
				t.Fatal("failed source observation allowed cleanup")
			}
			assertTestFile(t, filepath.Join(external, "sentinel"), "preserve")
		})
	}
}

func TestVerifyRejectsSecondSourceAppearingAfterJudgment(t *testing.T) {
	for _, selected := range []string{"current", "legacy", "compat"} {
		t.Run(selected, func(t *testing.T) {
			e := probeEnv(t)
			app, id := seedVerifyApp(t, e)
			legacyAuditFixture(t, e)
			candidates := e.auditCandidates(app, id)
			index := map[string]int{"current": 0, "legacy": 1, "compat": 2}[selected]
			upper := filepath.Join(candidates[index].path, "upper")
			writeTestFile(t, filepath.Join(upper, ".cache", "TestApp", "blob"), "original")
			record := filepath.Join(e.AuditDir(app, id), "run.toml")
			seedRunRecord(t, record, runRecord{App: app, ID: id})
			second := filepath.Join(candidates[(index+1)%len(candidates)].path, "upper", "sentinel")
			old := auditEvidenceTestHook
			fired := false
			auditEvidenceTestHook = func(stage string) {
				if stage == "after-judge" {
					fired = true
					writeTestFile(t, second, "new evidence")
				}
			}
			t.Cleanup(func() { auditEvidenceTestHook = old })
			if _, err := e.Verify(app, id); err == nil || !fired {
				t.Fatal("second source appeared but selected evidence was consumed")
			}
			assertTestFile(t, filepath.Join(upper, ".cache", "TestApp", "blob"), "original")
			assertTestFile(t, second, "new evidence")
			if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "summary.toml")); !os.IsNotExist(err) {
				t.Fatal("wrote summary for ambiguous sources")
			}
		})
	}
}

func TestVerifyRejectsSourceReappearingAfterDisposal(t *testing.T) {
	for _, compat := range []bool{false, true} {
		for _, leak := range []bool{false, true} {
			t.Run(strconv.FormatBool(compat)+"/leak="+strconv.FormatBool(leak), func(t *testing.T) {
				e := probeEnv(t)
				app, id := seedVerifyApp(t, e)
				work := e.AuditWorkDir(app, id)
				if compat {
					work = e.AuditDir(app, id)
				}
				upper := filepath.Join(work, "upper")
				rel := ".cache/TestApp/blob"
				if leak {
					rel = ".config/Unrecognized/state"
				}
				writeTestFile(t, filepath.Join(upper, rel), "original")
				record := filepath.Join(e.AuditDir(app, id), "run.toml")
				seedRunRecord(t, record, runRecord{App: app, ID: id})
				before, err := os.ReadFile(record)
				if err != nil {
					t.Fatal(err)
				}
				old := auditEvidenceTestHook
				fired := false
				auditEvidenceTestHook = func(stage string) {
					if stage == "after-dispose" {
						fired = true
						writeTestFile(t, filepath.Join(upper, "sentinel"), "new evidence")
					}
				}
				t.Cleanup(func() { auditEvidenceTestHook = old })
				if _, err := e.Verify(app, id); err == nil || !fired {
					t.Fatal("reappearing source certified consumed")
				}
				assertTestFile(t, record, string(before))
				assertTestFile(t, filepath.Join(upper, "sentinel"), "new evidence")
				if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "summary.toml")); !os.IsNotExist(err) {
					t.Fatal("wrote summary after source reappeared")
				}
			})
		}
	}
}

func TestForkRechecksIdentityAfterAvailability(t *testing.T) {
	for _, change := range []string{"instance", "base", "fork-mount"} {
		t.Run(change, func(t *testing.T) {
			e, p, in, abs := snapshotFixture(t)
			writeTestFile(t, filepath.Join(abs, "state"), "main")
			table := forkTableFixture(t)
			m := &sceneFaultMounter{base: table, available: func() {
				switch change {
				case "instance":
					replaceTestDir(t, in.Dir())
				case "base":
					replaceTestDir(t, e.Base)
				case "fork-mount":
					writeTestFile(t, filepath.Join(in.Dir(), ".fork", "sentinel"), "preserve")
					table.points = []string{filepath.Join(in.Dir(), ".fork")}
					table.save()
				}
			}}
			e.Mounter = m
			if _, _, _, err := e.forkMounts(p, in, nil, nil); err == nil || m.mounts != 0 || table.unmountCalls != 0 {
				t.Fatal("availability change accessed stale instance or mounted")
			}
			if change == "fork-mount" {
				assertTestFile(t, filepath.Join(in.Dir(), ".fork", "sentinel"), "preserve")
			}
		})
	}
}

func TestFailedCopySourceDriftKeepsPartialDestination(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary permission checks")
	}
	s := markerScene(t)
	upper := filepath.Join(s.path, "upper")
	writeTestFile(t, filepath.Join(upper, "a-sentinel"), "preserve")
	writeTestFile(t, filepath.Join(upper, "z-unreadable"), "unreadable")
	if err := os.Chmod(filepath.Join(upper, "z-unreadable"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(upper, "z-unreadable"), 0600)
		_ = os.Chmod(filepath.Join(upper+".old", "z-unreadable"), 0600)
	})
	if err := s.captureUpper(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "quarantine")
	oldRename, oldHook := moveRename, moveTestHook
	fired := false
	moveRename = func(string, string) error { return syscall.EXDEV }
	moveTestHook = func(stage string) {
		if stage != "copy-failed" {
			return
		}
		fired = true
		if err := os.Rename(upper, upper+".old"); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(upper, "replacement"), "new")
	}
	t.Cleanup(func() { moveRename, moveTestHook = oldRename, oldHook })
	err := moveSceneDir(s, s.upper, dst)
	if err == nil || dstComplete(err) || !fired || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe partial-copy result: %v fired=%v", err, fired)
	}
	assertTestFile(t, filepath.Join(dst, "a-sentinel"), "preserve")
	assertTestFile(t, filepath.Join(upper, "replacement"), "new")
	assertTestFile(t, filepath.Join(upper+".old", "a-sentinel"), "preserve")
}

func TestVerifyCompleteCopyRequiresOriginalResidualIdentity(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary permission checks")
	}
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			e := probeEnv(t)
			app, id := seedVerifyApp(t, e)
			upper := filepath.Join(e.AuditDir(app, id), "upper")
			locked := filepath.Join(upper, "locked")
			writeTestFile(t, filepath.Join(locked, "sentinel"), "preserve")
			if err := os.Chmod(locked, 0555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0700); _ = os.Chmod(filepath.Join(upper+".old", "locked"), 0700) })
			seedRunRecord(t, filepath.Join(e.AuditDir(app, id), "run.toml"), runRecord{App: app, ID: id})
			oldRename, oldChmod, oldHook := moveRename, removeChmod, auditEvidenceTestHook
			moveRename = func(string, string) error { return syscall.EXDEV }
			removeChmod = func(string, os.FileMode) error { return syscall.EPERM }
			fired := false
			auditEvidenceTestHook = func(stage string) {
				if stage != "after-dispose" {
					return
				}
				fired = true
				if replace {
					if err := os.Rename(upper, upper+".old"); err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, filepath.Join(upper, "replacement"), "new")
				}
			}
			t.Cleanup(func() { moveRename, removeChmod, auditEvidenceTestHook = oldRename, oldChmod, oldHook })
			v, err := e.Verify(app, id)
			if !fired {
				t.Fatal("did not reach complete-copy disposition")
			}
			if replace {
				if err == nil {
					t.Fatal("replaced residual source certified retained")
				}
				assertTestFile(t, filepath.Join(upper, "replacement"), "new")
				if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "summary.toml")); !os.IsNotExist(err) {
					t.Fatal("wrote summary after replacing residual")
				}
			} else if err != nil || !v.Retained || v.Quarantine == "" {
				t.Fatalf("unchanged residual lost complete copy: %+v %v", v, err)
			}
			qs, err := filepath.Glob(filepath.Join(e.AuditDir(app, id), "quarantine-*"))
			if err != nil || len(qs) != 1 {
				t.Fatal("backup missing")
			}
			assertTestFile(t, filepath.Join(qs[0], "locked", "sentinel"), "preserve")
		})
	}
}

func TestKnownProbeActiveDoesNotRecaptureAfterCheck(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(strconv.FormatBool(present), func(t *testing.T) {
			s := markerScene(t)
			marker := filepath.Join(s.path, "active.json")
			owner, err := identifySessionProcess(os.Getpid(), 0)
			if err != nil {
				t.Fatal(err)
			}
			boot, err := readProbeBootID()
			if err != nil {
				t.Fatal(err)
			}
			if present {
				inactive := owner
				inactive.Start++
				b, _ := json.Marshal(probeActiveRecord{Version: 1, BootID: boot, Owner: inactive})
				writeTestFile(t, marker, string(b))
			}
			if s.probeActive() {
				t.Fatal("synthetic scene should be inactive")
			}
			oldState := s.active
			oldHook := sceneTestHook
			fired := false
			sceneTestHook = func(stage, path string) {
				if stage != "after-active-check" || path != s.path {
					return
				}
				fired = true
				if present {
					if err := os.Rename(marker, marker+".old"); err != nil {
						t.Fatal(err)
					}
				}
				b, _ := json.Marshal(probeActiveRecord{Version: 1, BootID: boot, Owner: owner})
				writeTestFile(t, marker, string(b))
			}
			t.Cleanup(func() { sceneTestHook = oldHook })
			if s.probeActive() || !fired || s.active != oldState {
				t.Fatal("frozen active state was reread or replaced")
			}
			if s.remove(".") == nil {
				t.Fatal("new active marker accepted by subsequent cleanup")
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("new marker lost")
			}
		})
	}
}

func TestActiveTemporaryFailureBlocksOuterCleanup(t *testing.T) {
	for _, mount := range []bool{false, true} {
		t.Run(strconv.FormatBool(mount), func(t *testing.T) {
			table := forkTableFixture(t)
			e := probeEnv(t)
			var tmp, work string
			old := sceneTestHook
			sceneTestHook = func(stage, path string) {
				if stage != "before-active-publish" {
					return
				}
				tmp = path
				work = filepath.Dir(path)
				if mount {
					table.points = []string{path}
					table.save()
				} else {
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, path, "replacement")
				}
				writeTestFile(t, filepath.Join(work, "upper", "sentinel"), "preserve")
			}
			t.Cleanup(func() { sceneTestHook = old })
			if _, err := e.newProbeWorkScene("app"); err == nil || tmp == "" {
				t.Fatal("unsafe temporary marker allowed scene creation")
			}
			assertTestFile(t, filepath.Join(work, "upper", "sentinel"), "preserve")
			if _, err := os.Lstat(tmp); err != nil {
				t.Fatal("unsafe temporary marker deleted during failure cleanup")
			}
		})
	}
}

func TestWholeSceneCopyKeepsTrackedRemovalState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary permission checks")
	}
	for _, denied := range []bool{false, true} {
		t.Run(strconv.FormatBool(denied), func(t *testing.T) {
			e := probeEnv(t)
			s, err := e.newProbeWorkScene("app")
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			writeTestFile(t, filepath.Join(s.path, "upper", "state"), "original")
			locked := filepath.Join(s.path, "zz-locked")
			writeTestFile(t, filepath.Join(locked, "sentinel"), "preserve")
			if err := os.Chmod(locked, 0555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
			oldRename, oldChmod := moveRename, removeChmod
			moveRename = func(string, string) error { return syscall.EXDEV }
			if denied {
				removeChmod = func(string, os.FileMode) error { return syscall.EPERM }
			}
			t.Cleanup(func() { moveRename, removeChmod = oldRename, oldChmod })
			q, err := e.quarantineScene(s)
			if err != nil || q == "" {
				t.Fatalf("complete whole-scene backup not reported: q=%q err=%v", q, err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(q) }) // Only this synthetic quarantine belongs to the test.
			assertTestFile(t, filepath.Join(q, "upper", "state"), "original")
			assertTestFile(t, filepath.Join(q, "zz-locked", "sentinel"), "preserve")
			if _, err := os.Stat(filepath.Join(q, "active.json")); err != nil {
				t.Fatal("complete backup lost marker")
			}
			if denied {
				if s.absent || s.active != nil || !s.activeKnown || !s.upper.absent {
					t.Fatal("partial self-removal states not preserved")
				}
				assertTestFile(t, filepath.Join(locked, "sentinel"), "preserve")
				if err := s.checkUnmounted(); err != nil {
					t.Fatalf("own child removal seen as drift: %v", err)
				}
			} else if !s.absent {
				t.Fatal("permission retry did not finish source deletion")
			}
		})
	}
}

func TestWholeSceneChildReappearanceIsNotOwnRemoval(t *testing.T) {
	for _, child := range []string{"upper", "active.json"} {
		t.Run(child, func(t *testing.T) {
			e := probeEnv(t)
			s, err := e.newProbeWorkScene("app")
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			writeTestFile(t, filepath.Join(s.path, "upper", "original"), "preserve")
			dst := filepath.Join(t.TempDir(), "copy")
			oldRename, oldHook := moveRename, sceneTestHook
			moveRename = func(string, string) error { return syscall.EXDEV }
			fired := false
			var sentinel string
			sceneTestHook = func(stage, path string) {
				if stage != "after-scene-children" || path != s.path || fired {
					return
				}
				fired = true
				sentinel = filepath.Join(path, child)
				if child == "upper" {
					sentinel = filepath.Join(sentinel, "new")
				}
				writeTestFile(t, sentinel, "replacement")
			}
			t.Cleanup(func() { moveRename, sceneTestHook = oldRename, oldHook })
			err = moveSceneDir(s, s, dst)
			if err == nil || dstComplete(err) || !fired {
				t.Fatalf("new child accepted as removed: %v", err)
			}
			assertTestFile(t, sentinel, "replacement")
			assertTestFile(t, filepath.Join(dst, "upper", "original"), "preserve")
		})
	}
}
