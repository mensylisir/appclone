package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func replaceTestDir(t *testing.T, path string) string {
	t.Helper()
	saved := path + ".saved"
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(path, "replacement"), "untouched")
	return saved
}

func TestForkActualSourceDriftStopsLaunchAndCleanup(t *testing.T) {
	for _, phase := range []string{"after-mount", "before-build", "before-start", "before-fork-cleanup"} {
		for _, kind := range []string{"instance", "ancestor", "merged", "upper", "mount-record"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				e, p, in, abs := snapshotFixture(t)
				writeTestFile(t, filepath.Join(abs, "state"), "original")
				fm := newRecordingMounter(t)
				e.Mounter = fm
				counter := filepath.Join(t.TempDir(), "started")
				if err := os.WriteFile(p.Exec, []byte("#!/bin/sh\nprintf started > '"+counter+"'\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				oldMount := procMountInfo
				defer func() { procMountInfo = oldMount }()
				fired := false
				var saved string
				e.forkTestHook = func(stage string) {
					if stage != phase || fired {
						return
					}
					fired = true
					path := in.Dir()
					switch kind {
					case "ancestor":
						path = filepath.Dir(path)
					case "merged", "mount-record":
						path = filepath.Join(forkDir(in), forkHash(abs))
					case "upper":
						path, _ = instanceChild(in.Root, abs)
					}
					saved = replaceTestDir(t, path)
					if kind == "mount-record" {
						procMountInfo = filepath.Join(t.TempDir(), "mountinfo")
						writeTestFile(t, procMountInfo, fmt.Sprintf("1 2 0:3 / / rw - ext4 root rw\n3 1 0:4 / %s/merged rw - fuse.overlayfs old rw\n4 1 0:5 / %s/merged rw - fuse.overlayfs replacement rw\n", saved, path))
					}
				}
				if phase == "before-fork-cleanup" {
					_, cleanup, _, err := e.forkMounts(p, in, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := cleanup(); err == nil {
						t.Fatal("cleanup accepted replaced source")
					}
				} else {
					_, err := e.Run(context.Background(), p.App, in.ID, RunOptions{})
					if err == nil {
						t.Fatal("launch accepted replaced source")
					}
				}
				if !fired || fm.unmountCall != 0 {
					t.Fatalf("unsafe cleanup: fired=%v unmounts=%d", fired, fm.unmountCall)
				}
				if _, err := os.Stat(saved); err != nil {
					t.Fatalf("old scene removed: %v", err)
				}
				if _, err := os.Stat(counter); !os.IsNotExist(err) {
					t.Fatalf("target started: %v", err)
				}
			})
		}
	}
}

func TestForkCleanupRootDrift(t *testing.T) {
	for _, phase := range []string{"before-fork-reset", "before-generation-cleanup"} {
		for _, kind := range []string{"root", "ancestor"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				e, p, in, abs := snapshotFixture(t)
				writeTestFile(t, filepath.Join(abs, "state"), "original")
				writeTestFile(t, filepath.Join(forkDir(in), "sentinel"), "preserve")
				fm := newRecordingMounter(t)
				e.Mounter = fm
				var saved string
				e.forkTestHook = func(stage string) {
					if stage != phase || saved != "" {
						return
					}
					path := in.Dir()
					if phase == "before-generation-cleanup" {
						path = filepath.Join(path, "fork-base")
						writeTestFile(t, filepath.Join(path, ".stage-012345678901234567890123", "sentinel"), "preserve")
					}
					if kind == "ancestor" {
						path = filepath.Dir(path)
					}
					saved = replaceTestDir(t, path)
				}
				_, cleanup, _, err := e.forkMounts(p, in, nil, nil)
				defer cleanup()
				if err == nil || saved == "" || fm.unmountCall != 0 {
					t.Fatalf("cleanup accepted changed root: %v", err)
				}
				if _, err := os.Stat(saved); err != nil {
					t.Fatal("old tree removed")
				}
			})
		}
	}
}

func TestForkAllUppersRejectSymlinksBeforeCreation(t *testing.T) {
	for _, kind := range []string{"empty", "missing", "credential", "file"} {
		for _, ancestor := range []bool{false, true} {
			for _, inside := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/ancestor=%v/inside=%v", kind, ancestor, inside), func(t *testing.T) {
					e, p, in, abs := snapshotFixture(t)
					if kind == "missing" || kind == "file" {
						if err := os.Remove(abs); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "file" {
						writeTestFile(t, abs, "synthetic")
					}
					if kind == "credential" {
						p.CredentialPaths = p.Paths
					}
					link, _ := instanceChild(in.Root, abs)
					if ancestor {
						link = filepath.Dir(link)
					}
					dest := filepath.Join(t.TempDir(), "outside")
					if inside {
						dest = filepath.Join(in.Root, "other-app")
					}
					writeTestFile(t, filepath.Join(dest, "sentinel"), "preserve")
					if err := os.Rename(link, link+".saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(dest, link); err != nil {
						t.Fatal(err)
					}
					fm := newRecordingMounter(t)
					e.Mounter = fm
					_, cleanup, _, err := e.forkMounts(p, in, nil, nil)
					defer cleanup()
					if err == nil || len(fm.calls) != 0 {
						t.Fatal("unsafe upper accepted")
					}
					assertTestFile(t, filepath.Join(dest, "sentinel"), "preserve")
					ents, err := os.ReadDir(dest)
					if err != nil || len(ents) != 1 {
						t.Fatal("wrote through upper alias")
					}
				})
			}
		}
	}
}

func TestForkHashCollisionStopsBeforeSnapshot(t *testing.T) {
	paths := []string{"/home/example/.config/state-73254", "/home/example/.config/state-103832"}
	if forkHash(paths[0]) != forkHash(paths[1]) || checkForkHashes(paths) == nil {
		t.Fatal("collision fixture not rejected")
	}
	e, p, in, _ := snapshotFixture(t)
	p.Paths = paths
	fm := newRecordingMounter(t)
	e.Mounter = fm
	_, cleanup, _, err := e.forkMounts(p, in, nil, nil)
	defer cleanup()
	if err == nil || len(fm.calls) != 0 {
		t.Fatal("collision reached mount")
	}
	if _, err := os.Stat(filepath.Join(in.Dir(), "fork-base")); !os.IsNotExist(err) {
		t.Fatal("collision created snapshot")
	}
}

func TestRunPhaseRejectsUnsafeLock(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "replaced", "plain"} {
		t.Run(kind, func(t *testing.T) {
			e, _, in, _ := snapshotFixture(t)
			path := filepath.Join(in.Dir(), "run.lock")
			sentinel := filepath.Join(t.TempDir(), "outside")
			writeTestFile(t, sentinel, "preserve")
			switch kind {
			case "symlink":
				if err := os.Symlink(sentinel, path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := e.HoldRunLock(in.App, in.ID)
			if kind == "plain" || kind == "replaced" {
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if kind == "replaced" {
					if err := os.Rename(path, path+".saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(sentinel, path); err != nil {
						t.Fatal(err)
					}
				}
				err = writeRunPhase(lock, "preparing")
			}
			if (err == nil) != (kind == "plain") {
				t.Fatalf("lock acceptance %s: %v", kind, err)
			}
			assertTestFile(t, sentinel, "preserve")
		})
	}
}

func TestForkPIDWriteFailureStopsPreparation(t *testing.T) {
	e, p, in, _ := snapshotFixture(t)
	if err := os.Mkdir(filepath.Join(in.Dir(), "run.pid"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.forkTestHook = func(stage string) { t.Fatalf("preparation reached %s", stage) }
	if _, err := e.Run(context.Background(), p.App, in.ID, RunOptions{}); err == nil {
		t.Fatal("fork continued without stop record")
	}
	if _, err := os.Stat(filepath.Join(in.Dir(), "fork-base")); !os.IsNotExist(err) {
		t.Fatal("snapshot prepared without stop record")
	}
}
