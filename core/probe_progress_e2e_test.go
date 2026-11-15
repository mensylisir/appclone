package core

import (
	"context"
	"errors"
	"io"
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
	for _, mode := range []string{"complete", "cancel", "finish"} {
		t.Run(mode, func(t *testing.T) {
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
			control := &ProbeControl{}
			capture := &probeCaptureRunner{}
			finished := map[int]bool{}
			res, err := e.Probe(ctx, "progressdemo", ProbeOptions{Command: []string{app}, Control: control, Runner: capture,
				Progress: func(p ProbeProgress) {
					stages = append(stages, p.Stage)
					if p.Stage == "round_running" && p.Elapsed >= time.Second {
						live[p.Round] = p.ReadAvailable && p.Reads > 0
						if mode == "finish" && p.CanFinish {
							finished[p.Round] = control.Finish(p.SessionToken)
						}
						if mode == "cancel" {
							cancel()
						}
					}
				},
			})
			if mode == "cancel" {
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
				if mode == "finish" && (res.ReadTrace != ReadTracePartial || !finished[1] || !finished[2] || !containsString(res.ReadsWarns, "manual_finish")) {
					t.Fatalf("manual evidence: trace=%s finished=%v warns=%v", res.ReadTrace, finished, res.ReadsWarns)
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
			for _, p := range capture.processes {
				if !sessionGroupGone(p.PGID) {
					t.Fatalf("session group remains: %d", p.PGID)
				}
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

// The wrapper records process identities without changing the real runner path.
type probeCaptureRunner struct{ processes []SessionProcess }

func (r *probeCaptureRunner) Run(ctx context.Context, argv []string, w io.Writer) (SessionResult, error) {
	return r.RunWithStart(ctx, argv, w, nil)
}
func (r *probeCaptureRunner) RunWithStart(ctx context.Context, argv []string, w io.Writer, start func(SessionProcess) error) (SessionResult, error) {
	return (setsidRunner{}).RunWithStart(ctx, argv, w, func(p SessionProcess) error {
		r.processes = append(r.processes, p)
		if start != nil {
			return start(p)
		}
		return nil
	})
}
