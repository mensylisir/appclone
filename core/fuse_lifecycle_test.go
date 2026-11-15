package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func markerScene(t *testing.T) *overlayScene {
	t.Helper()
	s, err := openOverlayScene(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}

func TestFuseStartingBlocksCleanupUntilCompleted(t *testing.T) {
	forkTableFixture(t)
	s := markerScene(t)
	writeTestFile(t, filepath.Join(s.path, "sentinel"), "preserve")
	m, err := beginFuseStarting(s)
	if err != nil {
		t.Fatal(err)
	}
	if s.remove(".") == nil {
		t.Fatal("active startup scene was removed")
	}
	if _, err := beginFuseStarting(s); err == nil {
		t.Fatal("active startup marker was replaced")
	}
	assertTestFile(t, filepath.Join(s.path, "sentinel"), "preserve")
	if err := m.complete(); err != nil {
		t.Fatal(err)
	}
	if err := s.remove("."); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatal("completed startup scene was not removed")
	}
}

func TestFuseStartingMalformedMarkersPreserveScene(t *testing.T) {
	boot, err := readProbeBootID()
	if err != nil {
		t.Fatal(err)
	}
	valid := fuseStartingRecord{Version: 1, BootID: boot, Nonce: strings.Repeat("01", 12)}
	for _, kind := range []string{"same-boot", "version", "uuid", "nonce", "trailing", "unknown", "oversize", "fifo", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			forkTableFixture(t)
			s := markerScene(t)
			path := filepath.Join(s.path, fuseStartingName)
			writeTestFile(t, filepath.Join(s.path, "sentinel"), "preserve")
			rec := valid
			switch kind {
			case "version":
				rec.Version = 2
			case "uuid":
				rec.BootID = "bad-boot"
			case "nonce":
				rec.Nonce = "00"
			}
			data, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "trailing":
				data = append(data, []byte(" {}")...)
			case "unknown":
				data = append(data[:len(data)-1], []byte(",\"extra\":true}")...)
			case "oversize":
				data = append(data, []byte(strings.Repeat(" ", 4097))...)
			}
			switch kind {
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			case "symlink":
				target := filepath.Join(t.TempDir(), "marker")
				writeTestFile(t, target, string(data))
				err = os.Symlink(target, path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			default:
				err = os.WriteFile(path, data, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.remove(".") == nil {
				t.Fatal("unconfirmed marker accepted")
			}
			assertTestFile(t, filepath.Join(s.path, "sentinel"), "preserve")
		})
	}
}

func TestFuseStartingPreviousBootNeedsUnmountedScene(t *testing.T) {
	m := forkTableFixture(t)
	s := markerScene(t)
	boot, err := readProbeBootID()
	if err != nil {
		t.Fatal(err)
	}
	other := "00000000-0000-0000-0000-000000000001"
	if other == boot {
		other = "00000000-0000-0000-0000-000000000002"
	}
	data, _ := json.Marshal(fuseStartingRecord{Version: 1, BootID: other, Nonce: strings.Repeat("ab", 12)})
	writeTestFile(t, filepath.Join(s.path, fuseStartingName), string(data))
	m.points = []string{filepath.Join(s.path, "merged")}
	m.save()
	if s.remove(".") == nil {
		t.Fatal("previous-boot marker bypassed a remaining mount")
	}
	m.points = nil
	m.save()
	if err := s.remove("."); err != nil {
		t.Fatal(err)
	}
}

func TestFuseStartingCompleteRejectsReplacement(t *testing.T) {
	for _, kind := range []string{"inode", "nonce", "parent"} {
		t.Run(kind, func(t *testing.T) {
			forkTableFixture(t)
			s := markerScene(t)
			m, err := beginFuseStarting(s)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.path, fuseStartingName)
			switch kind {
			case "inode":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(m.record)
				writeTestFile(t, path, string(data))
			case "nonce":
				rec := m.record
				rec.Nonce = strings.Repeat("cd", 12)
				data, _ := json.Marshal(rec)
				writeTestFile(t, path, string(data))
			case "parent":
				replaceTestDir(t, s.path)
				writeTestFile(t, path, "replacement")
			}
			if m.complete() == nil {
				t.Fatal("changed startup identity accepted")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("replacement marker was removed")
			}
		})
	}
}

// Fake foreground driver/helper processes only touch an artificial mount table.
// The helper inherits the driver's group and closes all inherited pipes so Wait
// can reap the direct child while an actual live helper remains.
func TestFuseDriverRetainsHelperUntilReboot(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(strconv.FormatBool(stop), func(t *testing.T) {
			m := forkTableFixture(t)
			s := markerScene(t)
			merged := filepath.Join(s.path, "merged")
			if err := os.Mkdir(merged, 0o700); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			helperFile := filepath.Join(bin, "helper.pid")
			argsFile := filepath.Join(bin, "args")
			t.Setenv("FUSE_TEST_HELPER", helperFile)
			t.Setenv("FUSE_TEST_ARGS", argsFile)
			t.Setenv("FUSE_TEST_TABLE", m.table)
			t.Setenv("FUSE_TEST_EMPTY", "1 2 0:3 / / rw - ext4 root rw\n")
			m.points = []string{merged}
			m.save()
			mounted, err := os.ReadFile(m.table)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("FUSE_TEST_MOUNTED", string(mounted))
			m.points = nil
			m.save()
			body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FUSE_TEST_ARGS\"\n/bin/sleep 20 </dev/null >/dev/null 2>&1 &\nprintf '%s' \"$!\" > \"$FUSE_TEST_HELPER\"\n"
			if stop {
				body += "printf '%s' \"$FUSE_TEST_MOUNTED\" > \"$FUSE_TEST_TABLE\"\nexec /bin/sleep 20\n"
			} else {
				body += "exit 1\n"
			}
			for name, script := range map[string]string{
				"fuse-overlayfs": body,
				"fusermount3":    "#!/bin/sh\nprintf '%s' \"$FUSE_TEST_EMPTY\" > \"$FUSE_TEST_TABLE\"\n",
			} {
				path := filepath.Join(bin, name)
				writeTestFile(t, path, script)
				if err := os.Chmod(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			t.Cleanup(func() {
				data, err := os.ReadFile(helperFile)
				if err == nil {
					pid, err := strconv.Atoi(string(data))
					if err == nil && pid > 0 {
						p, _ := os.FindProcess(pid)
						_ = p.Kill()
					}
				}
			})
			unmount, err := (fuseOverlayMounter{}).Mount("lower", "upper", "work", merged)
			if stop {
				if err != nil {
					t.Fatal(err)
				}
				err = unmount()
				if err == nil || unmount() == nil {
					t.Fatal("stop forgot an unreaped helper or swallowed cached error")
				}
			}
			if err == nil || s.remove(".") == nil {
				t.Fatal("live helper did not preserve startup marker and scene")
			}
			if _, err := os.Stat(filepath.Join(s.path, fuseStartingName)); err != nil {
				t.Fatal("helper protection missing")
			}
			args, err := os.ReadFile(argsFile)
			if err != nil || strings.Contains(string(args), "auto_unmount") {
				t.Fatal("driver must not create detached auto_unmount helper")
			}
		})
	}
}

// A trusted synthetic mount record is sufficient for readiness. The missing
// merged path proves the driver does not perform a synchronous FUSE Stat.
func TestFuseDriverReadinessUsesRecordWithoutStat(t *testing.T) {
	m := forkTableFixture(t)
	s := markerScene(t)
	merged := filepath.Join(s.path, "merged-never-created")
	m.points = []string{merged}
	m.save()
	mounted, err := os.ReadFile(m.table)
	if err != nil {
		t.Fatal(err)
	}
	m.points = nil
	m.save()
	bin := t.TempDir()
	t.Setenv("FUSE_TEST_TABLE", m.table)
	t.Setenv("FUSE_TEST_MOUNTED", string(mounted))
	t.Setenv("FUSE_TEST_EMPTY", "1 2 0:3 / / rw - ext4 root rw\n")
	for name, body := range map[string]string{
		"fuse-overlayfs": "#!/bin/sh\nprintf '%s' \"$FUSE_TEST_MOUNTED\" > \"$FUSE_TEST_TABLE.new\"\n/bin/mv \"$FUSE_TEST_TABLE.new\" \"$FUSE_TEST_TABLE\"\nexec /bin/sleep 20\n",
		"fusermount3":    "#!/bin/sh\nprintf '%s' \"$FUSE_TEST_EMPTY\" > \"$FUSE_TEST_TABLE.new\"\n/bin/mv \"$FUSE_TEST_TABLE.new\" \"$FUSE_TEST_TABLE\"\n",
	} {
		path := filepath.Join(bin, name)
		writeTestFile(t, path, body)
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	stop, err := (fuseOverlayMounter{}).Mount("lower", "upper", "work", merged)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if _, err := os.Lstat(merged); !os.IsNotExist(err) {
		t.Fatal("fixture unexpectedly created merged")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if err := s.checkUnmounted(); err != nil {
		t.Fatal(err)
	}
}
