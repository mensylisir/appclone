package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in: real bwrap/fuse/strace, exclusively in a temporary HOME.
// A missing backend is a failure when this acceptance test is requested.
func TestProbeLiveProgressE2E(t *testing.T) {
	if os.Getenv("APPCLONE_PROBE_LIVE_E2E") != "1" {
		t.Skip("set APPCLONE_PROBE_LIVE_E2E=1 for real probe acceptance")
	}
	for _, cancelDuringRound := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[cancelDuringRound], func(t *testing.T) {
			e := probeEnv(t)
			e.UID, e.Resolver.UID = os.Getuid(), os.Getuid()
			e.ProbeCapsOverride = nil
			if err := os.MkdirAll(e.Base, 0o700); err != nil {
				t.Fatal(err)
			}
			e.Resolver.XDG["RUNTIME_DIR"] = filepath.Join(t.TempDir(), "run")
			if err := os.MkdirAll(e.Resolver.XDG["RUNTIME_DIR"], 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", e.Resolver.Home)
			t.Setenv("AC_HOME", e.Resolver.Home)
			t.Setenv("AC_BASE", e.Base)
			for key, value := range e.Resolver.XDG {
				t.Setenv("XDG_"+key, value)
			}
			cfg := filepath.Join(e.Resolver.Home, ".config", "Progress 演示")
			if err := os.MkdirAll(cfg, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg, "seed"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			app := filepath.Join(t.TempDir(), "progress 演示")
			body := "#!/bin/sh\ncat \"$HOME/.config/Progress 演示/seed\" >/dev/null\n" +
				"printf fixture >\"$HOME/.config/Progress 演示/written\"\n" +
				"printf shared >\"$HOME/Downloads/progress-shared\"\nsleep 2.2\n"
			if err := os.WriteFile(app, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			live := map[int]bool{}
			var stages []string
			res, err := e.Probe(ctx, "progressdemo", ProbeOptions{Command: []string{app},
				Progress: func(p ProbeProgress) {
					stages = append(stages, p.Stage)
					if p.Stage == "round_running" && p.Elapsed >= time.Second {
						live[p.Round] = p.ReadAvailable && p.Reads > 0
						if cancelDuringRound {
							cancel()
						}
					}
				},
			})
			if cancelDuringRound {
				if !errors.Is(err, ErrCanceled) {
					t.Fatalf("cancel: %v", err)
				}
				if !live[1] {
					t.Fatal("cancel happened without live read evidence")
				}
			} else {
				if err != nil {
					t.Fatalf("probe: %v (cause: %v)", err, errors.Unwrap(err))
				}
				if res.Profile.ProbeMethod != "overlay" || len(res.Profile.Paths) == 0 || !live[1] || !live[2] {
					t.Fatalf("incomplete overlay/round evidence: method=%s paths=%d live=%v", res.Profile.ProbeMethod, len(res.Profile.Paths), live)
				}
				if stages[len(stages)-1] != "round_done" {
					t.Fatal(stages)
				}
			}
			if _, err := os.Stat(filepath.Join(cfg, "written")); !os.IsNotExist(err) {
				t.Fatalf("host config was written: %v", err)
			}
			if _, err := os.Stat(filepath.Join(e.Resolver.Home, "Downloads", "progress-shared")); err != nil {
				t.Fatalf("shared file missing: %v", err)
			}
			items, err := os.ReadDir(e.TmpRoot)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if strings.HasPrefix(item.Name(), "appclone-probe-") {
					t.Fatalf("probe remains: %s", item.Name())
				}
			}
		})
	}
}
