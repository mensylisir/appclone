package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type missingRecordMounter struct{ unmountCalls int }

func (*missingRecordMounter) Available() error { return nil }
func (m *missingRecordMounter) Mount(_, _, _, merged string) (func() error, error) {
	return func() error { m.unmountCalls++; return nil }, nil
}

func TestForkSuccessfulMountNeedsRecord(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	writeTestFile(t, filepath.Join(abs, "state"), "original")
	forkTableFixture(t)
	m := &missingRecordMounter{}
	e.Mounter = m
	counter := filepath.Join(t.TempDir(), "started")
	writeTestFile(t, p.Exec, "#!/bin/sh\nprintf started > '"+counter+"'\n")
	_, err := e.Run(context.Background(), p.App, in.ID, RunOptions{})
	if err == nil || m.unmountCalls != 0 {
		t.Fatalf("unconfirmed mount accepted/cleaned: %v, %d", err, m.unmountCalls)
	}
	if _, err := os.Stat(filepath.Join(forkDir(in), "mount-pending")); err != nil {
		t.Fatal("unconfirmed mount protection was removed")
	}
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatal("target started without a mount")
	}
}

func TestForkDriverTimeoutDoesNotUnmount(t *testing.T) {
	forkTableFixture(t)
	merged := filepath.Join(t.TempDir(), "missing merged 中文")
	bin := t.TempDir()
	counter := filepath.Join(t.TempDir(), "unmounted")
	for name, body := range map[string]string{
		"fuse-overlayfs": "#!/bin/sh\nexec /bin/sleep 20\n",
		"fusermount3":    "#!/bin/sh\nprintf unsafe > '" + counter + "'\n",
		"fusermount":     "#!/bin/sh\nprintf unsafe > '" + counter + "'\n",
	} {
		file := filepath.Join(bin, name)
		writeTestFile(t, file, body)
		if err := os.Chmod(file, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	_, err := (fuseOverlayMounter{}).Mount("lower", "upper", "work", merged)
	if errKey(err) != "err.probe.mount_timeout" {
		t.Fatalf("did not exercise timeout: %v", err)
	}
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatal("driver unmounted an unknown mount")
	}
}

func forkBaseAlias(t *testing.T, e *Env) (string, string) {
	t.Helper()
	original := e.Base
	alias := filepath.Join(t.TempDir(), "storage alias 中文")
	if err := os.Symlink(original, alias); err != nil {
		t.Fatal(err)
	}
	e.Base = alias
	return alias, original
}

func redirectForkAlias(t *testing.T, alias, target string) {
	t.Helper()
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
}

func TestForkBaseAliasFrozenAcrossPreparation(t *testing.T) {
	for _, phase := range []string{"before-copy", "before-publish", "before-mount", "after-mount", "before-build", "before-start"} {
		t.Run(phase, func(t *testing.T) {
			e, p, in, abs := snapshotFixture(t)
			writeTestFile(t, filepath.Join(abs, "state"), "original")
			m := newRecordingMounter(t)
			e.Mounter = m
			alias, original := forkBaseAlias(t, e)
			replacement := t.TempDir()
			writeTestFile(t, filepath.Join(replacement, "sentinel"), "preserve")
			counter := filepath.Join(t.TempDir(), "started")
			writeTestFile(t, p.Exec, "#!/bin/sh\nprintf started > '"+counter+"'\n")
			fired := false
			e.forkTestHook = func(stage string) {
				if stage == phase && !fired {
					fired = true
					redirectForkAlias(t, alias, replacement)
				}
			}
			_, err := e.Run(context.Background(), p.App, in.ID, RunOptions{})
			if err == nil || !fired || m.unmountCall != 0 {
				t.Fatalf("alias drift accepted: %v, %v, %d", err, fired, m.unmountCall)
			}
			if e.Base != alias {
				t.Fatal("caller Env was changed")
			}
			if _, err := os.Stat(counter); !os.IsNotExist(err) {
				t.Fatal("target started")
			}
			entries, err := os.ReadDir(replacement)
			if err != nil || len(entries) != 1 {
				t.Fatal("wrote to replacement storage")
			}
			assertTestFile(t, filepath.Join(replacement, "sentinel"), "preserve")
			if _, err := os.Stat(original); err != nil {
				t.Fatal("original storage removed")
			}
			if phase == "before-copy" || phase == "before-publish" {
				if _, err := os.Stat(filepath.Join(in.Dir(), "fork-base", "current.toml")); !os.IsNotExist(err) {
					t.Fatal("published after alias change")
				}
			}
		})
	}
}

func TestForkInitialBaseAliasWorks(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	writeTestFile(t, filepath.Join(abs, "state"), "original")
	e.Mounter = newRecordingMounter(t)
	alias, original := forkBaseAlias(t, e)
	in.Root = filepath.Join(e.InstancePath(p.App, in.ID), "root")
	sources, cleanup, check, err := e.forkMounts(p, in, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !IsUnder(original, sources[abs]) || e.Base != alias {
		t.Fatal("storage not pinned privately")
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestForkRejectsInitialInstanceAliases(t *testing.T) {
	for _, level := range []string{"instances", "app", "id"} {
		for _, inside := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inside=%v", level, inside), func(t *testing.T) {
				e, p, in, abs := snapshotFixture(t)
				writeTestFile(t, filepath.Join(abs, "state"), "original")
				writeTestFile(t, filepath.Join(in.Dir(), "sentinel"), "preserve")
				m := newRecordingMounter(t)
				e.Mounter = m
				path := in.Dir()
				if level == "app" {
					path = filepath.Dir(path)
				}
				if level == "instances" {
					path = filepath.Dir(filepath.Dir(path))
				}
				dest := filepath.Join(t.TempDir(), "relocated")
				if inside {
					dest = filepath.Join(e.Base, "relocated")
				}
				if err := os.Rename(path, dest); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dest, path); err != nil {
					t.Fatal(err)
				}
				counter := filepath.Join(t.TempDir(), "started")
				writeTestFile(t, p.Exec, "#!/bin/sh\nprintf started > '"+counter+"'\n")
				if _, err := e.Run(context.Background(), p.App, in.ID, RunOptions{}); err == nil {
					t.Fatal("instance alias accepted")
				}
				if len(m.calls) != 0 {
					t.Fatal("mounted outside declared layout")
				}
				assertTestFile(t, filepath.Join(in.Dir(), "sentinel"), "preserve")
				for _, file := range []string{counter, filepath.Join(in.Dir(), "run.pid"), filepath.Join(in.Dir(), "run.lock"), filepath.Join(in.Dir(), "fork-base")} {
					if _, err := os.Stat(file); !os.IsNotExist(err) {
						t.Fatalf("unexpected preparation file: %s", file)
					}
				}
			})
		}
	}
}

func TestForkPinRejectsForgedInstance(t *testing.T) {
	e, _, in, _ := snapshotFixture(t)
	identity := e.captureForkRun(in.App, in.ID)
	e = e.withForkRun(identity)
	for _, change := range []func(*Instance){
		func(in *Instance) { in.Root = filepath.Join(in.Dir(), "other-root") },
		func(in *Instance) { in.Root = filepath.Join(t.TempDir(), "root") },
		func(in *Instance) { in.App = "another" },
		func(in *Instance) { in.ID = "another" },
	} {
		forged := in
		change(&forged)
		if _, _, err := e.pinForkInstance(forged); err == nil {
			t.Fatal("forged instance silently repinned")
		}
	}
}

func TestForkAliasChangeWhileWaitingForSchemaLock(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	writeTestFile(t, filepath.Join(abs, "state"), "original")
	m := newRecordingMounter(t)
	e.Mounter = m
	// Model an old profile without using SaveProfile's no-downgrade barrier.
	profile, err := os.ReadFile(e.ProfilePath(p.App))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, e.ProfilePath(p.App), strings.Replace(string(profile), "version = 2", "version = 1", 1))
	state, err := e.OpenState(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	alias, _ := forkBaseAlias(t, e)
	replacement := t.TempDir()
	entered := make(chan struct{})
	e.forkTestHook = func(stage string) {
		if stage == "before-profile-lock" {
			close(entered)
		}
	}
	done := make(chan error, 1)
	go func() { _, err := e.Run(context.Background(), p.App, in.ID, RunOptions{}); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("did not reach schema lock")
	}
	redirectForkAlias(t, alias, replacement)
	state.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("alias drift while waiting accepted")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("run did not stop")
	}
	if len(m.calls) != 0 {
		t.Fatal("mounted after storage changed")
	}
	entries, err := os.ReadDir(replacement)
	if err != nil || len(entries) != 0 {
		t.Fatal("new storage was modified")
	}
	if _, err := os.Stat(filepath.Join(in.Dir(), "fork-base")); !os.IsNotExist(err) {
		t.Fatal("snapshot created")
	}
	got, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(in.Dir()))), "profiles.d", p.App+".toml"))
	if err != nil || !strings.Contains(string(got), "version = 1") {
		t.Fatal("profile migrated after alias changed")
	}
}

func TestForkCancellationReportsCleanupFailure(t *testing.T) {
	for _, phase := range []string{"preparation", "before-start"} {
		for _, failure := range []string{"none", "unmount", "mountinfo"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				e, p, in, abs := snapshotFixture(t)
				writeTestFile(t, filepath.Join(abs, "state"), "original")
				other := filepath.Join(e.Resolver.Home, ".config", "Second")
				writeTestFile(t, filepath.Join(other, "state"), "second")
				p.Paths = append(p.Paths, other)
				if err := e.SaveProfile(p); err != nil {
					t.Fatal(err)
				}
				m := newRecordingMounter(t)
				e.Mounter = m
				if failure == "unmount" {
					m.unmountErr = errors.New("unmount failed")
				}
				cancelAt := func() {
					if failure == "mountinfo" {
						writeTestFile(t, m.table.table, "invalid\n")
					}
				}
				var err error
				if phase == "preparation" {
					_, _, _, err = e.forkMounts(p, in, func() bool {
						if len(m.calls) > 0 {
							cancelAt()
							return true
						}
						return false
					}, nil)
				} else {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					e.forkTestHook = func(stage string) {
						if stage == phase {
							cancelAt()
							cancel()
						}
					}
					_, err = e.Run(ctx, p.App, in.ID, RunOptions{})
				}
				if err == nil || errors.Is(err, ErrCanceled) != (failure == "none") {
					t.Fatalf("wrong cancellation result: %v", err)
				}
				if failure != "none" {
					if _, err := os.Stat(forkDir(in)); err != nil {
						t.Fatal("failed cleanup scene removed")
					}
				}
			})
		}
	}
}
