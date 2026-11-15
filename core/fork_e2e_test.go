package core

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real mounts, isolated synthetic state only. Opt-in requires the backend and
// optionally the preserved old binary; failures may not be reported as skips.
func TestForkSnapshotE2E(t *testing.T) {
	if os.Getenv("APPCLONE_FORK_E2E") != "1" {
		t.Skip("set APPCLONE_FORK_E2E=1 for real snapshot acceptance")
	}
	e, p, in, abs := snapshotFixture(t)
	e.UID, e.Resolver.UID = os.Getuid(), os.Getuid()
	t.Setenv("HOME", e.Resolver.Home)
	t.Setenv("AC_HOME", e.Resolver.Home)
	t.Setenv("AC_BASE", e.Base)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	for key, value := range e.Resolver.XDG {
		t.Setenv("XDG_"+key, value)
		if err := os.MkdirAll(value, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(abs, "state"), "original")
	writeTestFile(t, filepath.Join(abs, "deleted"), "original")
	public := filepath.Join(e.Resolver.Home, "Downloads")
	if err := os.MkdirAll(public, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `#!/bin/sh
set -eu
cfg="$HOME/.config/TestApp"
if [ "$1" = first ]; then
  test "$(cat "$cfg/state")" = original
  printf clone > "$cfg/state"
  rm "$cfg/deleted"
else
  test "$(cat "$cfg/state")" = clone
  test ! -e "$cfg/deleted"
fi
printf shared > "$HOME/Downloads/fork-shared"
`
	if err := os.WriteFile(p.Exec, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(arg string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var stderr strings.Builder
		res, err := e.Run(ctx, p.App, in.ID, RunOptions{Args: []string{arg}, Stderr: &stderr})
		if err != nil || res.AppExitCode != 0 {
			t.Fatalf("run %s: result=%+v error=%v cause=%v stderr=%s", arg, res, err, errors.Unwrap(err), stderr.String())
		}
		if mounted, err := checkedMountUnder(in.Dir()); err != nil || mounted {
			t.Fatalf("mount remains: %v %v", mounted, err)
		}
	}
	run("first")
	assertTestFile(t, filepath.Join(abs, "state"), "original")
	assertTestFile(t, filepath.Join(abs, "deleted"), "original")
	assertTestFile(t, filepath.Join(public, "fork-shared"), "shared")
	writeTestFile(t, filepath.Join(abs, "state"), "host-changed")
	run("second")
	assertTestFile(t, filepath.Join(abs, "state"), "host-changed")
	lower := snapshotForTest(t, e, p, in)[abs]
	assertTestFile(t, filepath.Join(lower, "state"), "original")
	upper, _ := instanceChild(in.Root, abs)
	assertTestFile(t, filepath.Join(upper, "state"), "clone")
	other, err := e.CreateInstanceWith(context.Background(), p.App, p, "other", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	otherLower := snapshotForTest(t, e, p, other)[abs]
	assertTestFile(t, filepath.Join(otherLower, "state"), "host-changed")
	if otherLower == lower {
		t.Fatal("clones share snapshot")
	}
	if old := os.Getenv("APPCLONE_OLD_BINARY"); old != "" {
		counter := filepath.Join(public, "old-started")
		if err := os.WriteFile(p.Exec, []byte("#!/bin/sh\nprintf started > '"+counter+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(old, "run", p.App, in.ID)
		out, err := cmd.CombinedOutput()
		if err == nil || (!strings.Contains(string(out), "version") && !strings.Contains(string(out), "版本")) {
			t.Fatalf("old binary did not reject schema: %v %s", err, out)
		}
		if _, err := os.Stat(counter); !os.IsNotExist(err) {
			t.Fatalf("old binary started target: %v", err)
		}
		t.Log("v1.0.4 rejected schema2; target starts=0")
	}
}
