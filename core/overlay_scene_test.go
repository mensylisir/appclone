package core

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOverlayScenePreservesAnyMount(t *testing.T) {
	for _, position := range []string{"root", "ancestor", "merged", "upper/hidden", "other", fuseStartingName} {
		t.Run(position, func(t *testing.T) {
			m := forkTableFixture(t)
			path := filepath.Join(t.TempDir(), "scene 中文")
			writeTestFile(t, filepath.Join(path, "sentinel"), "preserve")
			point := filepath.Join(path, position)
			if position == "root" {
				point = path
			} else if position == "ancestor" {
				point = filepath.Dir(path)
			}
			m.points = []string{point}
			m.save()
			if err := removeOverlayScene(path); err == nil {
				t.Fatal("mounted scene removed")
			}
			assertTestFile(t, filepath.Join(path, "sentinel"), "preserve")
		})
	}
}

func TestQuarantineFallbackKeepsOriginalSceneIdentity(t *testing.T) {
	e := probeEnv(t)
	forkTableFixture(t)
	src := filepath.Join(e.TmpRoot, "original-scene")
	writeTestFile(t, filepath.Join(src, "sentinel"), "original")
	scene, err := openOverlayScene(src)
	if err != nil {
		t.Fatal(err)
	}
	defer scene.close()
	old := moveRename
	calls := 0
	moveRename = func(from, to string) error {
		calls++
		if calls == 1 {
			if err := os.Rename(from, from+".old"); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(from, "sentinel"), "replacement")
			return syscall.EACCES
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { moveRename = old })
	if _, err := e.quarantineScene(scene); err == nil || calls != 1 {
		t.Fatal("fallback recaptured replacement scene")
	}
	assertTestFile(t, filepath.Join(src, "sentinel"), "replacement")
	assertTestFile(t, filepath.Join(src+".old", "sentinel"), "original")
}

func TestOrdinaryPathRejectsLinkToFuseAncestor(t *testing.T) {
	m := forkTableFixture(t)
	parent := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias 中文")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(alias, "scene")
	writeTestFile(t, filepath.Join(path, "sentinel"), "preserve")
	m.points = []string{parent}
	m.save()
	if _, err := openOverlayScene(path); err == nil {
		t.Fatal("followed a link into an unknown FUSE ancestor")
	}
	if _, err := openCopyDestination(filepath.Join(alias, "copy")); err == nil {
		t.Fatal("created a destination under FUSE alias")
	}
	assertTestFile(t, filepath.Join(path, "sentinel"), "preserve")
}

func TestForkSnapshotCleanupHonorsStartingMarker(t *testing.T) {
	for _, level := range []string{".fork", ".fork/12345678"} {
		t.Run(level, func(t *testing.T) {
			e, _, in, _ := snapshotFixture(t)
			forkTableFixture(t)
			identity := e.captureForkRun(in.App, in.ID)
			base := filepath.Join(in.Dir(), "fork-base")
			name := ".stage-" + strings.Repeat("ab", 12)
			writeTestFile(t, filepath.Join(base, name, "sentinel"), "preserve")
			root, err := openForkRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			work := filepath.Join(in.Dir(), level)
			if err := os.MkdirAll(work, 0o700); err != nil {
				t.Fatal(err)
			}
			scene, err := openOverlayScene(work)
			if err != nil {
				t.Fatal(err)
			}
			defer scene.close()
			marker, err := beginFuseStarting(scene)
			if err != nil {
				t.Fatal(err)
			}
			if err := removeForkStaging(root, base, name, identity); err == nil {
				t.Fatal("staging deleted while mount startup unresolved")
			}
			if err := e.withForkRun(identity).cleanForkGenerations(root, in.Dir(), forkManifest{}); err == nil {
				t.Fatal("generation scan ignored startup marker")
			}
			if err := resetForkBase(forkDir(in)); err == nil {
				t.Fatal("reset ignored startup marker")
			}
			assertTestFile(t, filepath.Join(base, name, "sentinel"), "preserve")
			if err := marker.complete(); err != nil {
				t.Fatal(err)
			}
			if err := removeForkStaging(root, base, name, identity); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOverlaySceneRejectsUnknownMountTable(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(strconv.FormatBool(missing), func(t *testing.T) {
			m := forkTableFixture(t)
			path := filepath.Join(t.TempDir(), "scene")
			writeTestFile(t, filepath.Join(path, "sentinel"), "preserve")
			if missing {
				if err := os.Remove(m.table); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTestFile(t, m.table, "bad mount table\n")
			}
			if err := removeOverlayScene(path); err == nil {
				t.Fatal("unknown mount state accepted")
			}
			assertTestFile(t, filepath.Join(path, "sentinel"), "preserve")
		})
	}
}

func TestOverlaySceneFreezesInitialParentAlias(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(strconv.FormatBool(drift), func(t *testing.T) {
			forkTableFixture(t)
			parent := t.TempDir()
			alias := filepath.Join(t.TempDir(), "parent alias 中文")
			if err := os.Symlink(parent, alias); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(alias, "scene")
			writeTestFile(t, filepath.Join(path, "sentinel"), "preserve")
			s, err := openOverlayScene(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			if drift {
				replacement := t.TempDir()
				writeTestFile(t, filepath.Join(replacement, "scene", "sentinel"), "new")
				redirectForkAlias(t, alias, replacement)
				if s.remove(".") == nil {
					t.Fatal("parent alias drift accepted")
				}
				assertTestFile(t, filepath.Join(path, "sentinel"), "new")
				assertTestFile(t, filepath.Join(parent, "scene", "sentinel"), "preserve")
			} else if err := s.remove("."); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOverlaySceneRejectsRootAliasAndReplacement(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(strconv.FormatBool(alias), func(t *testing.T) {
			forkTableFixture(t)
			path := filepath.Join(t.TempDir(), "scene")
			writeTestFile(t, filepath.Join(path, "sentinel"), "preserve")
			s, err := openOverlayScene(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			old := path + ".old"
			if err := os.Rename(path, old); err != nil {
				t.Fatal(err)
			}
			if alias {
				if err := os.Symlink(old, path); err != nil {
					t.Fatal(err)
				}
				if _, err := openOverlayScene(path); err == nil {
					t.Fatal("initial root alias accepted")
				}
			} else {
				writeTestFile(t, filepath.Join(path, "sentinel"), "new")
			}
			if s.remove(".") == nil {
				t.Fatal("scene replacement accepted")
			}
			assertTestFile(t, filepath.Join(old, "sentinel"), "preserve")
		})
	}
}

func TestMoveSceneRejectsMountAfterCopy(t *testing.T) {
	for _, location := range []string{"source", "sibling", "target"} {
		t.Run(location, func(t *testing.T) {
			m := forkTableFixture(t)
			s := markerScene(t)
			src := filepath.Join(s.path, "upper")
			dst := filepath.Join(t.TempDir(), "quarantine")
			writeTestFile(t, filepath.Join(src, "sentinel"), "preserve")
			oldRename, oldHook, oldChmod := moveRename, moveTestHook, removeChmod
			moveRename = func(string, string) error { return syscall.EXDEV }
			chmods := 0
			removeChmod = func(string, os.FileMode) error { chmods++; return nil }
			moveTestHook = func(stage string) {
				if stage != "after-copy" {
					return
				}
				point := filepath.Join(src, "hidden")
				if location == "sibling" {
					point = filepath.Join(s.path, "merged")
				} else if location == "target" {
					point = filepath.Join(dst, "hidden")
				}
				m.points = []string{point}
				m.save()
			}
			t.Cleanup(func() { moveRename, moveTestHook, removeChmod = oldRename, oldHook, oldChmod })
			source, err := openOverlayScene(src)
			if err != nil {
				t.Fatal(err)
			}
			defer source.close()
			err = moveSceneDir(s, source, dst)
			if err == nil || dstComplete(err) || chmods != 0 {
				t.Fatalf("unsafe scene consumed: %v (chmod=%d)", err, chmods)
			}
			assertTestFile(t, filepath.Join(src, "sentinel"), "preserve")
			assertTestFile(t, filepath.Join(dst, "sentinel"), "preserve")
		})
	}
}

type failedSceneMounter struct {
	t           *testing.T
	table       *forkTableMounter
	failMount   bool
	failUnmount bool
	scene       string
}

func (*failedSceneMounter) Available() error { return nil }
func (m *failedSceneMounter) Mount(lower, upper, work, merged string) (func() error, error) {
	m.scene = filepath.Dir(merged)
	stop, err := m.table.Mount(lower, upper, work, merged)
	if err != nil {
		return nil, err
	}
	if m.failMount {
		return nil, errors.New("injected error after mount")
	}
	return func() error {
		if m.failUnmount {
			return errors.New("injected unmount failure")
		}
		return stop()
	}, nil
}

func TestFuseSmokePreservesFailedMountAndUnmount(t *testing.T) {
	for _, mountFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(mountFailure), func(t *testing.T) {
			m := &failedSceneMounter{t: t, table: forkTableFixture(t), failMount: mountFailure, failUnmount: !mountFailure}
			if err := fuseSmokeAt(t.TempDir(), m); err == nil {
				t.Fatal("failed smoke accepted")
			}
			assertTestFile(t, filepath.Join(m.scene, "merged", "sentinel"), "preserve")
			if m.table.unmountCalls != 0 {
				t.Fatal("unexpected fallback unmount")
			}
		})
	}
}

func TestStaleSmokeRecoversOnlyKnownMerged(t *testing.T) {
	for _, kind := range []string{"fresh", "aged", "same-boot", "bad-marker", "extra-mount", "unmount-fails", "mount-replaced"} {
		t.Run(kind, func(t *testing.T) {
			m := forkTableFixture(t)
			root := t.TempDir()
			scenePath := filepath.Join(root, "appclone-fusesmoke-fixture")
			writeTestFile(t, filepath.Join(scenePath, "sentinel"), "preserve")
			scene, err := openOverlayScene(scenePath)
			if err != nil {
				t.Fatal(err)
			}
			defer scene.close()
			if kind == "same-boot" || kind == "fresh" {
				if _, err := beginFuseStarting(scene); err != nil {
					t.Fatal(err)
				}
			} else if kind == "bad-marker" {
				writeTestFile(t, filepath.Join(scenePath, fuseStartingName), "broken")
			}
			if kind != "fresh" {
				past := time.Now().Add(-time.Hour)
				if err := os.Chtimes(scenePath, past, past); err != nil {
					t.Fatal(err)
				}
			}
			m.points = []string{filepath.Join(scenePath, "merged")}
			if kind == "extra-mount" {
				m.points = append(m.points, filepath.Join(scenePath, "other"))
			}
			m.save()
			result := "1 2 0:3 / / rw - ext4 root rw\n"
			if kind == "mount-replaced" {
				data, err := os.ReadFile(m.table)
				if err != nil {
					t.Fatal(err)
				}
				result = strings.Replace(string(data), "20 1 0:30", "70 1 0:80", 1)
			}
			bin := t.TempDir()
			calls := filepath.Join(bin, "calls")
			t.Setenv("SMOKE_TEST_TABLE", m.table)
			t.Setenv("SMOKE_TEST_RESULT", result)
			t.Setenv("SMOKE_TEST_CALLS", calls)
			body := "#!/bin/sh\nprintf 'call\\n' >> \"$SMOKE_TEST_CALLS\"\n"
			if kind == "unmount-fails" {
				body += "exit 1\n"
			} else {
				body += "printf '%s' \"$SMOKE_TEST_RESULT\" > \"$SMOKE_TEST_TABLE\"\n"
			}
			script := filepath.Join(bin, "fusermount3")
			writeTestFile(t, script, body)
			if err := os.Chmod(script, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			err = sweepStaleSmoke(root)
			log, _ := os.ReadFile(calls)
			if kind == "aged" {
				if err != nil || strings.Count(string(log), "call") != 1 {
					t.Fatalf("known stale mount not recovered: %v calls=%q", err, log)
				}
				if _, err := os.Lstat(scenePath); !os.IsNotExist(err) {
					t.Fatal("unmounted scene remains")
				}
				return
			}
			if kind == "fresh" {
				if err != nil {
					t.Fatalf("fresh concurrent smoke blocked availability: %v", err)
				}
			} else if err == nil {
				t.Fatal("unsafe stale smoke accepted")
			}
			if kind != "unmount-fails" && kind != "mount-replaced" && len(log) != 0 {
				t.Fatal("protected scene was unmounted")
			}
			assertTestFile(t, filepath.Join(scenePath, "sentinel"), "preserve")
		})
	}
}
