package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This fixture models mount identity changes without mounting any real data.
type forkTableMounter struct {
	t            *testing.T
	table        string
	points       []string
	failAt       int
	unmountCalls int
	onUnmount    func()
}

func (m *forkTableMounter) Available() error { return nil }

func (m *forkTableMounter) save() {
	m.t.Helper()
	text := "1 2 0:3 / / rw - ext4 root rw\n"
	escape := strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`)
	for i, point := range m.points {
		if point != "" {
			text += fmt.Sprintf("%d 1 0:%d / %s rw - fuse.overlayfs fixture rw\n", 20+i, 30+i, escape.Replace(point))
		}
	}
	writeTestFile(m.t, m.table, text)
}

func (m *forkTableMounter) Mount(_, _, _, merged string) (func() error, error) {
	index := len(m.points)
	m.points = append(m.points, merged)
	writeTestFile(m.t, filepath.Join(merged, "sentinel"), "preserve")
	m.save()
	if len(m.points) == m.failAt {
		return nil, errors.New("mount returned before its identity was confirmed")
	}
	return func() error {
		m.unmountCalls++
		m.points[index] = ""
		m.save()
		if m.onUnmount != nil {
			m.onUnmount()
		}
		return nil
	}, nil
}

func forkTableFixture(t *testing.T) *forkTableMounter {
	t.Helper()
	original := procMountInfo
	m := &forkTableMounter{t: t, table: filepath.Join(t.TempDir(), "mountinfo")}
	m.save()
	procMountInfo = m.table
	t.Cleanup(func() { procMountInfo = original })
	return m
}

func TestForkPendingMountSurvivesRetry(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			e, p, in, abs := snapshotFixture(t)
			writeTestFile(t, filepath.Join(abs, "state"), "original")
			if count == 2 {
				other := filepath.Join(e.Resolver.Home, ".config", "Second App 中文")
				writeTestFile(t, filepath.Join(other, "state"), "second")
				p.Paths = append(p.Paths, other)
				if err := e.SaveProfile(p); err != nil {
					t.Fatal(err)
				}
			}
			m := forkTableFixture(t)
			m.failAt = count
			e.Mounter = m
			counter := filepath.Join(t.TempDir(), "started")
			writeTestFile(t, p.Exec, "#!/bin/sh\nprintf started > '"+counter+"'\n")
			// Any attempt by reset to invoke the real unmount tool is a failure.
			bin := t.TempDir()
			for _, name := range []string{"fusermount", "fusermount3"} {
				file := filepath.Join(bin, name)
				writeTestFile(t, file, "#!/bin/sh\nprintf unsafe > '"+counter+"'\nexit 1\n")
				if err := os.Chmod(file, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := e.Run(context.Background(), p.App, in.ID, RunOptions{}); err == nil {
					t.Fatal("unconfirmed mount accepted")
				}
				if m.unmountCalls != 0 || len(m.points) != count {
					t.Fatalf("retry changed mounts: calls=%d points=%d", m.unmountCalls, len(m.points))
				}
			}
			if err := resetForkBase(forkDir(in)); err == nil {
				t.Fatal("fresh reset accepted an unconfirmed mount")
			}
			for _, point := range m.points {
				assertTestFile(t, filepath.Join(point, "sentinel"), "preserve")
			}
			if _, err := os.Stat(counter); !os.IsNotExist(err) {
				t.Fatal("target or unmount tool started")
			}
			if _, err := os.Stat(filepath.Join(forkDir(in), "mount-pending")); err != nil {
				t.Fatal("persistent protection missing")
			}
			m.points = nil
			m.save()
			if err := resetForkBase(forkDir(in)); err != nil {
				t.Fatalf("no-mount recovery failed: %v", err)
			}
			if _, err := os.Stat(forkDir(in)); !os.IsNotExist(err) {
				t.Fatal("empty scene did not recover")
			}
		})
	}
}

func TestForkCleanupRechecksBetweenMounts(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprint(drift), func(t *testing.T) {
			e, p, in, abs := snapshotFixture(t)
			writeTestFile(t, filepath.Join(abs, "state"), "original")
			other := filepath.Join(e.Resolver.Home, ".config", "Second")
			writeTestFile(t, filepath.Join(other, "state"), "second")
			p.Paths = append(p.Paths, other)
			m := forkTableFixture(t)
			e.Mounter = m
			_, cleanup, _, err := e.forkMounts(p, in, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(forkDir(in), "mount-pending")); !os.IsNotExist(err) {
				t.Fatal("confirmed mount left pending marker")
			}
			if drift {
				m.onUnmount = func() {
					replaceTestDir(t, filepath.Dir(m.points[0]))
				}
			}
			err = cleanup()
			if drift {
				if err == nil || m.unmountCalls != 1 {
					t.Fatalf("second unmount did not validate identity: %v, %d", err, m.unmountCalls)
				}
			} else if err != nil || m.unmountCalls != 2 {
				t.Fatalf("normal cleanup failed: %v, %d", err, m.unmountCalls)
			}
		})
	}
}

func TestForkPendingMarkerCannotFollowAlias(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			e, p, in, abs := snapshotFixture(t)
			writeTestFile(t, filepath.Join(abs, "state"), "original")
			m := newRecordingMounter(t)
			e.Mounter = m
			external := filepath.Join(t.TempDir(), "sentinel")
			writeTestFile(t, external, "preserve")
			e.forkTestHook = func(stage string) {
				if stage == "before-mount" {
					path := filepath.Join(forkDir(in), "mount-pending")
					var err error
					if kind == "directory" {
						err = os.Mkdir(path, 0o700)
					} else {
						err = os.Symlink(external, path)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			_, _, _, err := e.forkMounts(p, in, nil, nil)
			if err == nil || len(m.calls) != 0 {
				t.Fatal("mounted without persistent protection")
			}
			assertTestFile(t, external, "preserve")
		})
	}
}

func TestForkMountIdentityChangePreservesScene(t *testing.T) {
	e, p, in, abs := snapshotFixture(t)
	writeTestFile(t, filepath.Join(abs, "state"), "original")
	m := forkTableFixture(t)
	e.Mounter = m
	_, cleanup, check, err := e.forkMounts(p, in, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.table)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, m.table, strings.Replace(string(data), "20 1 0:30", "90 1 0:99", 1))
	if check() == nil || cleanup() == nil || m.unmountCalls != 0 {
		t.Fatal("replacement mount was accepted or unmounted")
	}
	assertTestFile(t, filepath.Join(m.points[0], "sentinel"), "preserve")
}
