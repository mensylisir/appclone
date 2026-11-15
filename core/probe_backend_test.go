package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeRequiresPrivateWriteLayer(t *testing.T) {
	for _, key := range []string{"err.probe.no_fuse_overlayfs", "err.probe.no_fusermount", "err.probe.no_dev_fuse", "err.probe.mount_failed"} {
		for _, strace := range []bool{false, true} {
			t.Run(key+map[bool]string{false: "/no-strace", true: "/strace"}[strace], func(t *testing.T) {
				e := probeEnv(t)
				bin := t.TempDir()
				if strace {
					if err := os.WriteFile(filepath.Join(bin, "strace"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("PATH", bin)
				state := filepath.Join(e.Resolver.Home, "primary-state")
				if err := os.WriteFile(state, []byte("synthetic"), 0o600); err != nil {
					t.Fatal(err)
				}
				before, _ := os.Stat(state)
				r := &fakeRunner{}
				m := &fakeMounter{availableErr: errEnv(key, nil, nil)}
				_, err := e.Probe(context.Background(), "demo", ProbeOptions{
					Command: []string{"/synthetic/application"}, Mounter: m, Runner: r,
				})
				if ProbeErrorKey(err) != key || r.calls != 0 || m.mounted {
					t.Fatalf("unsafe backend: err=%v calls=%d mounted=%v", err, r.calls, m.mounted)
				}
				after, serr := os.Stat(state)
				if serr != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
					t.Fatal("primary state changed")
				}
			})
		}
	}
}

func TestLegacyStraceProfileStillLoads(t *testing.T) {
	e := probeEnv(t)
	p := Profile{App: "legacy", Version: 1, Exec: "/usr/bin/true", Paths: []string{"~/.config/Legacy"}, ProbeMethod: "strace"}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	got, err := e.LoadProfile(p.App)
	if err != nil || got.ProbeMethod != "strace" || len(got.Paths) != 1 {
		t.Fatalf("legacy profile rejected: %+v %v", got, err)
	}
}
