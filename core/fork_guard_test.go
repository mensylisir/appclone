package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForkRejectsHistoricalAliasChanges(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	writeTestFile(t, filepath.Join(abs, "state"), "original")
	snapshotForTest(t, e, p, in)
	other := filepath.Join(t.TempDir(), "new-source")
	writeTestFile(t, filepath.Join(other, "state"), "other")
	if err := os.Rename(abs, abs+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, abs); err != nil {
		t.Fatal(err)
	}
	paths, err := normalizePaths(p.Paths, e.BuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.prepareForkSnapshot(p, in, paths, nil, nil)
	if errKey(err) != "err.fork.scope_changed" {
		t.Fatalf("alias refresh accepted: %v", err)
	}
	// Removing the declaration does not release its historical identity.
	saved := p.Paths
	p.Paths = nil
	snapshotForTest(t, e, p, in)
	p.Paths = saved
	_, err = e.prepareForkSnapshot(p, in, paths, nil, nil)
	if errKey(err) != "err.fork.scope_changed" {
		t.Fatalf("re-added alias refresh accepted: %v", err)
	}
}

func TestForkAliasDriftCannotPublish(t *testing.T) {
	for _, phase := range []string{"before-copy", "before-publish"} {
		for _, existing := range []bool{false, true} {
			for _, kind := range []string{"owner", "ancestor", "tool-overlap", "home"} {
				t.Run(phase+"/"+kind+map[bool]string{false: "/fresh", true: "/existing"}[existing], func(t *testing.T) {
					e, p, in, abs := snapshotFixture(t)
					writeTestFile(t, filepath.Join(abs, "state"), "original")
					manifest := filepath.Join(in.Dir(), "fork-base", "current.toml")
					var old []byte
					if existing {
						snapshotForTest(t, e, p, in)
						var err error
						old, err = os.ReadFile(manifest)
						if err != nil {
							t.Fatal(err)
						}
						abs = filepath.Join(e.Resolver.Home, ".config", "Additional")
						p.Paths = append(p.Paths, abs)
						writeTestFile(t, filepath.Join(abs, "state"), "original")
					}
					paths, err := normalizePaths(p.Paths, e.BuildOptions())
					if err != nil {
						t.Fatal(err)
					}
					link := abs
					other := filepath.Join(t.TempDir(), "alternate")
					if kind != "owner" {
						link = filepath.Dir(abs)
					}
					if kind == "home" {
						link = e.Resolver.Home
					}
					if kind == "tool-overlap" {
						other = e.Base
					}
					state := filepath.Join(other, "state")
					if kind != "owner" {
						state = filepath.Join(other, filepath.Base(abs), "state")
					}
					if kind == "home" {
						state = filepath.Join(other, ".config", filepath.Base(abs), "state")
					}
					writeTestFile(t, state, "wrong")
					fired := false
					e.forkTestHook = func(stage string) {
						if stage != phase || fired {
							return
						}
						fired = true
						if err := os.Rename(link, link+".saved"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(other, link); err != nil {
							t.Fatal(err)
						}
					}
					_, err = e.prepareForkSnapshot(p, in, paths, nil, nil)
					if !fired || err == nil {
						t.Fatalf("drift accepted: fired=%v err=%v", fired, err)
					}
					if existing {
						assertTestFile(t, manifest, string(old))
					} else if _, err := os.Stat(manifest); !os.IsNotExist(err) {
						t.Fatalf("first manifest published: %v", err)
					}
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(link+".saved", link); err != nil {
						t.Fatal(err)
					}
					e.forkTestHook = nil
					lower := snapshotForTest(t, e, p, in)[abs]
					assertTestFile(t, filepath.Join(lower, "state"), "original")
				})
			}
		}
	}
}

func TestForkLateDriftNeverStartsTarget(t *testing.T) {
	for _, phase := range []string{"after-mount", "before-build", "before-start"} {
		t.Run(phase, func(t *testing.T) {
			e, p, in, abs := snapshotFixture(t)
			counter := filepath.Join(t.TempDir(), "started")
			if err := os.WriteFile(p.Exec, []byte("#!/bin/sh\nprintf started > '"+counter+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(abs, "state"), "original")
			e.Mounter = newRecordingMounter(t)
			other := filepath.Join(t.TempDir(), "other")
			writeTestFile(t, filepath.Join(other, "state"), "wrong")
			fired := false
			e.forkTestHook = func(stage string) {
				if stage != phase || fired {
					return
				}
				fired = true
				if err := os.Rename(abs, abs+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, abs); err != nil {
					t.Fatal(err)
				}
			}
			_, err := e.Run(context.Background(), p.App, in.ID, RunOptions{})
			if !fired || err == nil {
				t.Fatalf("late drift accepted: %v %v", fired, err)
			}
			if _, err := os.Stat(counter); !os.IsNotExist(err) {
				t.Fatalf("target started: %v", err)
			}
		})
	}
}

func TestForkMountTargetsRejectSymlinkReplacement(t *testing.T) {
	for _, part := range []string{"upper", "work", "merged"} {
		for _, ancestor := range []bool{false, true} {
			t.Run(part+map[bool]string{false: "/root", true: "/ancestor"}[ancestor], func(t *testing.T) {
				e, p, in, abs := snapshotFixture(t)
				writeTestFile(t, filepath.Join(abs, "state"), "original")
				fm := newRecordingMounter(t)
				e.Mounter = fm
				outside := filepath.Join(t.TempDir(), "outside")
				writeTestFile(t, filepath.Join(outside, "sentinel"), "untouched")
				fired := false
				e.forkTestHook = func(stage string) {
					if stage != "before-mount" || fired {
						return
					}
					fired = true
					path := filepath.Join(forkDir(in), forkHash(abs), part)
					if part == "upper" {
						path, _ = instanceChild(in.Root, abs)
					}
					if ancestor {
						path = filepath.Dir(path)
					}
					if err := os.Rename(path, path+".saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				}
				_, cleanup, _, err := e.forkMounts(p, in, nil, nil)
				defer cleanup()
				if !fired || err == nil || len(fm.calls) != 0 {
					t.Fatalf("unsafe mount accepted: %v %v %d", fired, err, len(fm.calls))
				}
				assertTestFile(t, filepath.Join(outside, "sentinel"), "untouched")
				entries, err := os.ReadDir(outside)
				if err != nil || len(entries) != 1 {
					t.Fatalf("wrote outside: %v %d", err, len(entries))
				}
			})
		}
	}
}

func TestForkCleanupRequiresTrustedMountInfo(t *testing.T) {
	original := procMountInfo
	defer func() { procMountInfo = original }()
	for _, body := range []string{
		"missing", "malformed\n", "1 2 0:1 / relative rw - ext4 x rw\n",
		"1 2 invalid-device / / rw - ext4 root rw\n",
		"1 2 0:x / / rw - ext4 root rw\n",
		"1 2 0:1 / /bad\\041path rw - ext4 root rw\n",
		"1 2 0:1 / /bad/../path rw - ext4 root rw\n",
		"1 2 0:1 / / rw - ext4  rw\n",
		"1 2 0:1 /bad\\ / rw - ext4 root rw\n",
	} {
		base := filepath.Join(t.TempDir(), ".fork")
		writeTestFile(t, filepath.Join(base, "merged", "sentinel"), "preserve")
		procMountInfo = filepath.Join(t.TempDir(), "mountinfo")
		if body != "missing" {
			writeTestFile(t, procMountInfo, body)
		}
		if err := resetForkBase(base); err == nil {
			t.Fatal("untrusted mountinfo permitted cleanup")
		}
		assertTestFile(t, filepath.Join(base, "merged", "sentinel"), "preserve")
	}
	writeTestFile(t, procMountInfo, "77 51 0:5 net:[4026532757] /run/netns/test rw - nsfs nsfs rw\n")
	if _, err := checkedMountPoints(); err != nil {
		t.Fatalf("valid nsfs rejected: %v", err)
	}
}

func TestForkBindingTableCannotSilentlyFallBack(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	o := e.BuildOptions()
	o.ForkSources = map[string]string{}
	if _, err := BuildBwrapArgs(p, in, o); errKey(err) != "err.fork.scope_changed" {
		t.Fatalf("missing bind accepted: %v", err)
	}
	upper, _ := instanceChild(in.Root, abs)
	o.ForkSources[abs] = upper
	args, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, "|"), "--bind|"+upper+"|"+abs) {
		t.Fatal("explicit upper missing")
	}
}
