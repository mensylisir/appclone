package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProbeSceneRejectsMalformedMarkers(t *testing.T) {
	for _, kind := range []string{"fifo", "directory", "symlink", "trailing", "oversized", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "active.json")
			owner, _ := identifySessionProcess(os.Getpid(), 0)
			owner.Start++
			b, _ := json.Marshal(probeActiveRecord{Version: 1, Owner: owner})
			switch kind {
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("/dev/null", path); err != nil {
					t.Fatal(err)
				}
			case "trailing":
				b = append(b, []byte(" corrupt")...)
			case "oversized":
				b = append(b, []byte(strings.Repeat(" ", 5000))...)
			case "unknown":
				b = []byte(`{"Version":1,"unknown":true}`)
			}
			if kind == "trailing" || kind == "oversized" || kind == "unknown" {
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			if !probeSceneActive(root) {
				t.Fatal("untrusted marker accepted")
			}
			if time.Since(start) > time.Second {
				t.Fatal("marker check blocked")
			}
		})
	}
}

func TestProbeOwnershipFailureRetainsUnknownSession(t *testing.T) {
	r := setsidRunner{waitLimit: time.Second, identify: func(int, int) (SessionProcess, error) {
		return SessionProcess{}, errors.New("injected identity failure")
	}}
	res, err := r.Run(context.Background(), []string{"/bin/sleep", "20"}, io.Discard)
	if err == nil || !res.Started || !res.Unreaped {
		t.Fatalf("unknown descendants accepted: %+v %v", res, err)
	}
	select {
	case <-res.Reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("direct process not reaped")
	}
}

func TestProbeScenePersistsAcrossCLIProcess(t *testing.T) {
	if os.Getenv("APPCLONE_TEST_ACTIVE_SCENE") != "" {
		if !probeSceneActive(os.Getenv("APPCLONE_TEST_ACTIVE_SCENE")) {
			t.Fatal("child process lost protection")
		}
		return
	}
	root := t.TempDir()
	target, _ := identifySessionProcess(os.Getpid(), os.Getpid())
	owner := target
	owner.PGID = 0
	owner.Start++ // parent is gone, controlled process remains
	b, _ := json.Marshal(probeActiveRecord{Version: 1, Owner: owner, Target: &target})
	if err := os.WriteFile(filepath.Join(root, "active.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProbeScenePersistsAcrossCLIProcess$")
	cmd.Env = append(os.Environ(), "APPCLONE_TEST_ACTIVE_SCENE="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestProbeBackgroundReapUnlocksOnlyAfterExit(t *testing.T) {
	scene := markerScene(t)
	root := scene.lexical
	cmd := exec.Command("/bin/sleep", "20")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	p, err := identifySessionProcess(cmd.Process.Pid, cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeProbeActive(scene, false, &p); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	cleaned := make(chan struct{})
	retainUnreapedProbe(scene, SessionResult{Process: p, Reaped: reaped, Unreaped: true}, func() error { close(cleaned); return nil })
	t.Cleanup(func() { unreapedProbes.Delete(root) })
	if !probeSessionsBlocked() || !probeSceneActive(root) {
		t.Fatal("live scene lost protection")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	close(reaped)
	select {
	case <-cleaned:
	case <-time.After(3 * time.Second):
		t.Fatal("background cleanup not reached")
	}
	deadline := time.Now().Add(time.Second)
	for probeSessionsBlocked() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if probeSessionsBlocked() || probeSceneActive(root) {
		t.Fatal("completed session remains blocked")
	}
}

func TestProbeBootIdentityRecovery(t *testing.T) {
	boot := "01234567-1234-1234-1234-0123456789ab"
	other := "01234567-1234-1234-1234-0123456789ac"
	owner, _ := identifySessionProcess(os.Getpid(), 0)
	for _, tc := range []struct {
		name, record, current string
		want                  bool
	}{
		{"same", boot, boot, true}, {"different", boot, other, false},
		{"legacy", "", boot, true}, {"bad-record", "bad", boot, true},
		{"read-failure", boot, "", true}, {"bad-current", boot, "invalid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeRecordActive(probeActiveRecord{Version: 1, Owner: owner, Launching: true, BootID: tc.record}, tc.current)
			if got != tc.want {
				t.Fatalf("active=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestProbeCleanupFailurePreservesOwnerProtection(t *testing.T) {
	scene := markerScene(t)
	root := scene.lexical
	owner, _ := identifySessionProcess(os.Getpid(), 0)
	exited := SessionProcess{PID: 2147483647, PGID: 2147483647, Start: 1, UID: os.Getuid()}
	if err := writeProbeActive(scene, false, &exited); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	tried := make(chan struct{})
	// PGID uses a definitely absent positive group; never signals it.
	exited.PGID = 2147483647
	retainUnreapedProbe(scene, SessionResult{Process: exited, Reaped: done}, func() error { close(tried); return errors.New("injected cleanup failure") })
	t.Cleanup(func() { unreapedProbes.Delete(root) })
	select {
	case <-tried:
	case <-time.After(time.Second):
		t.Fatal("cleanup not attempted")
	}
	if !probeSessionsBlocked() || !probeSceneActive(root) {
		t.Fatal("cleanup failure released protection")
	}
	boot, err := readProbeBootID()
	if err != nil {
		t.Fatal(err)
	}
	record := probeActiveRecord{Version: 1, Owner: owner, Target: &exited, BootID: boot}
	if !probeRecordActive(record, boot) {
		t.Fatal("active owner bypassed")
	}
	record.Owner.Start++
	if probeRecordActive(record, boot) {
		t.Fatal("dead owner and target cannot recover")
	}
}

func TestProbeObserverFailureReapsBeforeReturning(t *testing.T) {
	scene := markerScene(t)
	root := scene.lexical
	defer os.Chmod(root, 0700)
	r := setsidRunner{grace: 50 * time.Millisecond, waitLimit: time.Second, identify: func(pid, pgid int) (SessionProcess, error) {
		p, err := identifySessionProcess(pid, pgid)
		if err == nil {
			err = os.Chmod(root, 0500)
		}
		return p, err
	}}
	c := &ProbeControl{}
	res, err := runProbeRound(context.Background(), r, []string{"/bin/sleep", "20"}, io.Discard, ProbeOptions{Control: c, work: root, scene: scene}, nil, 0, 1)
	if ProbeErrorKey(err) != "err.probe.session_state" || res.Unreaped || !res.Started || !sessionGroupGone(res.Process.PGID) {
		t.Fatalf("observer failure cleanup: %+v %v", res, err)
	}
	if token, enabled := c.state(); enabled || token != 0 {
		t.Fatal("failed observer exposed finish token")
	}
}
