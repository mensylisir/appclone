package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func snapshotFixture(t *testing.T) (*Env, Profile, Instance, string) {
	t.Helper()
	e := testEnv(t)
	app, p := seedApp(t, e)
	p.Exec = filepath.Join(t.TempDir(), "快照 应用")
	if err := os.WriteFile(p.Exec, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	in, err := e.CreateInstanceWith(context.Background(), app, p, "main", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err = e.promoteForkProfile(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := e.Resolver.Expand(p.Paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return e, p, in, abs
}

func snapshotForTest(t *testing.T, e *Env, p Profile, in Instance) map[string]string {
	t.Helper()
	paths, err := normalizePaths(p.Paths, e.BuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	creds, err := e.forkCredentials(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.prepareForkSnapshot(p, in, paths, creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestForkSnapshotFixedAndUpperPreserved(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	writeTestFile(t, filepath.Join(abs, "中文 state"), "original")
	upper, _ := instanceChild(in.Root, abs)
	writeTestFile(t, filepath.Join(upper, "private"), "clone")
	writeTestFile(t, filepath.Join(upper, ".wh.deleted"), "")
	first := snapshotForTest(t, e, p, in)[abs]
	writeTestFile(t, filepath.Join(abs, "中文 state"), "changed")
	second := snapshotForTest(t, e, p, in)[abs]
	if first != second {
		t.Fatal("fixed generation changed")
	}
	assertTestFile(t, filepath.Join(second, "中文 state"), "original")
	assertTestFile(t, filepath.Join(upper, "private"), "clone")
	if _, err := os.Stat(filepath.Join(upper, ".wh.deleted")); err != nil {
		t.Fatal(err)
	}
	// A separate clone receives its own first-run starting point.
	other, err := e.CreateInstanceWith(context.Background(), p.App, p, "other", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	third := snapshotForTest(t, e, p, other)[abs]
	if third == first {
		t.Fatal("clones share a snapshot")
	}
	assertTestFile(t, filepath.Join(third, "中文 state"), "changed")
	// Removing then adding an owner must keep the same starting point.
	saved := p.Paths
	p.Paths = nil
	snapshotForTest(t, e, p, in)
	p.Paths = saved
	if got := snapshotForTest(t, e, p, in)[abs]; got != first {
		t.Fatal("re-added owner refreshed")
	}
}

func TestForkSnapshotEmptyMissingFileAndCredentialsStayDecided(t *testing.T) {
	for _, kind := range []string{"empty", "missing", "file", "credential"} {
		t.Run(kind, func(t *testing.T) {
			e, p, in, abs := snapshotFixture(t)
			switch kind {
			case "missing":
				if err := os.Remove(abs); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.Remove(abs); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, abs, "single")
			case "credential":
				p.CredentialPaths = []string{p.Paths[0]}
				writeTestFile(t, filepath.Join(abs, "credentials"), "synthetic")
			}
			if len(snapshotForTest(t, e, p, in)) != 0 {
				t.Fatal("unexpected inheritance")
			}
			if kind == "file" {
				if err := os.Remove(abs); err != nil {
					t.Fatal(err)
				}
			}
			writeTestFile(t, filepath.Join(abs, "later"), "later")
			p.CredentialPaths = nil
			if len(snapshotForTest(t, e, p, in)) != 0 {
				t.Fatal("permanent decision refreshed")
			}
		})
	}
}

func TestForkSnapshotCurrentCredentialsAndOverlappingOwners(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	writeTestFile(t, filepath.Join(abs, "state"), "state")
	if len(snapshotForTest(t, e, p, in)) != 1 {
		t.Fatal("snapshot missing")
	}
	p.CredentialPaths = []string{filepath.Join(abs, "new-token")}
	if len(snapshotForTest(t, e, p, in)) != 0 {
		t.Fatal("new credential scope did not block old snapshot")
	}
	for _, path := range []string{filepath.Dir(abs), filepath.Join(abs, "child")} {
		p.Paths = []string{path}
		p.CredentialPaths = nil
		_, err := e.prepareForkSnapshot(p, in, []string{path}, nil, nil)
		if errKey(err) != "err.fork.scope_changed" {
			t.Fatalf("overlap accepted: %v", err)
		}
	}
}

func TestForkCopyFailuresAndSymlinks(t *testing.T) {
	for _, mode := range []string{"fallback", "space", "change", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			source, target := filepath.Join(t.TempDir(), "源 数据"), filepath.Join(t.TempDir(), "snapshot")
			writeTestFile(t, filepath.Join(source, "data"), "first")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/outside/not-read", filepath.Join(source, "external")); err != nil {
				t.Fatal(err)
			}
			src, err := openForkRoot(source)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			dst, err := openForkRoot(target)
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()
			before, err := forkInventory(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			canceled := false
			clone := func(int, int) error {
				switch mode {
				case "space":
					return unix.ENOSPC
				case "change":
					writeTestFile(t, filepath.Join(source, "data"), "other")
				case "cancel":
					canceled = true
				}
				return unix.EOPNOTSUPP
			}
			err = copyForkTree(src, dst, before, func() bool { return canceled }, clone)
			if mode == "fallback" {
				if err != nil {
					t.Fatal(err)
				}
				assertTestFile(t, filepath.Join(target, "data"), "first")
				if link, err := os.Readlink(filepath.Join(target, "external")); err != nil || link != "/outside/not-read" {
					t.Fatalf("link changed: %q %v", link, err)
				}
			} else if err == nil {
				t.Fatal("failure accepted")
			}
			if mode == "space" && !errors.Is(err, unix.ENOSPC) {
				t.Fatalf("disk full retried: %v", err)
			}
			if mode == "cancel" && !errors.Is(err, ErrCanceled) {
				t.Fatalf("cancel ignored: %v", err)
			}
		})
	}
}

func TestForkSchemaCannotBeDowngradedByStaleViews(t *testing.T) {
	e, p, _, _ := snapshotFixture(t)
	p.Version = 1
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateInstance(context.Background(), p.App, p, "stale"); err != nil {
		t.Fatal(err)
	}
	got, err := e.LoadProfile(p.App)
	if err != nil || got.Version != 2 {
		t.Fatalf("schema downgraded: %d %v", got.Version, err)
	}
	merged := MergeNonPathFields(Profile{Version: 1}, got)
	if merged.Version != 2 {
		t.Fatal("rediscover dropped schema")
	}
}

func TestForkSnapshotCancelAndCorruptManifest(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	writeTestFile(t, filepath.Join(abs, "state"), "state")
	_, err := e.prepareForkSnapshot(p, in, []string{abs}, nil, func() bool { return true })
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancel=%v", err)
	}
	manifest := filepath.Join(in.Dir(), "fork-base", "current.toml")
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatalf("canceled snapshot published: %v", err)
	}
	snapshotForTest(t, e, p, in)
	original, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"version=9\npaths=[]\n", "version=1\npaths=[]\nunknown=true\n", "version=1\n[[paths]]\npath=\"/x\"\nkind=\"snapshot\"\ndata=\"../../escape\"\n"} {
		writeTestFile(t, manifest, body)
		if _, err := e.prepareForkSnapshot(p, in, []string{abs}, nil, nil); err == nil {
			t.Fatal("corrupt manifest accepted")
		}
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside")
	writeTestFile(t, external, string(original))
	if err := os.Symlink(external, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareForkSnapshot(p, in, []string{abs}, nil, nil); err == nil {
		t.Fatal("symlink manifest accepted")
	}
	assertTestFile(t, external, string(original))
}

func TestPreparingStatusRequiresHeldRunLock(t *testing.T) {
	e, p, in, _ := snapshotFixture(t)
	lock, err := e.HoldRunLock(p.App, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writeRunPhase(lock, "preparing"); err != nil {
		t.Fatal(err)
	}
	if !e.IsPreparing(p.App, in.ID) {
		t.Fatal("preparation not visible")
	}
	if err := writeRunPhase(lock, "running"); err != nil {
		t.Fatal(err)
	}
	if e.IsPreparing(p.App, in.ID) {
		t.Fatal("ready still preparing")
	}
	if err := writeRunPhase(lock, "preparing"); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if e.IsPreparing(p.App, in.ID) {
		t.Fatal("stale lock contents are not running state")
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
func assertTestFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("synthetic fixture mismatch: %s (%v)", path, err)
	}
}
