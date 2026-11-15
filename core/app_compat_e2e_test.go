package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A manually driven, opt-in acceptance fixture for installed applications.
// design/app-compat.sh owns the temporary HOME and test bus. Nothing below
// overrides capabilities, tracing, mounting or process supervision.
func TestAppCompatProbeE2E(t *testing.T) {
	if os.Getenv("APPCLONE_APP_COMPAT") != "1" {
		t.Skip("set APPCLONE_APP_COMPAT=1 via design/app-compat.sh")
	}
	root := os.Getenv("APPCLONE_COMPAT_ROOT")
	if filepath.Dir(root) != "/tmp" || !strings.HasPrefix(filepath.Base(root), "appclone-w4-") {
		t.Fatal("acceptance requires its own /tmp/appclone-w4-* root")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatal("fixture root must be a private ordinary directory")
	}
	owned, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	openedInfo, err := owned.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		t.Fatal("fixture root changed while opening")
	}
	fixed := map[string]os.FileInfo{}
	for _, part := range []string{"home", "base", "control", "work", "run"} {
		path := filepath.Join(root, part)
		compatDirectory(t, owned, root, path)
		fixed[path], err = os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	markerInfo, err := owned.Lstat(".fixture")
	if err != nil || !markerInfo.Mode().IsRegular() {
		t.Fatal("fixture marker must be an ordinary file")
	}
	marker, err := owned.ReadFile(".fixture")
	if err != nil || string(marker) != "appclone compatibility fixture v1\n" {
		t.Fatal("not an initialized acceptance fixture")
	}
	e, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.Resolver.Home != filepath.Join(root, "home") || os.Getenv("HOME") != e.Resolver.Home ||
		os.Getenv("AC_HOME") != e.Resolver.Home || e.Base != filepath.Join(root, "base") {
		t.Fatal("refusing non-fixture HOME/AC_HOME/AC_BASE")
	}
	for k, value := range e.Resolver.XDG {
		if !IsUnder(root, value) || !IsUnder(root, Real(value)) {
			t.Fatalf("XDG %s is outside the fixture", k)
		}
		compatDirectory(t, owned, root, value)
		fixed[value], err = os.Lstat(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	app := os.Getenv("APPCLONE_COMPAT_APP")
	e.AppDirList = []string{filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications")}
	e.TmpRoot = filepath.Join(root, "work")
	d, err := e.LoadApp(app)
	if err != nil {
		t.Fatal(err)
	}
	controlPath, err := os.MkdirTemp(filepath.Join(root, "control"), "probe-")
	if err != nil {
		t.Fatal(err)
	}
	controlDir, err := owned.OpenRoot(filepath.Join("control", filepath.Base(controlPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer controlDir.Close()
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signals, 10*time.Minute)
	defer cancel()
	state, err := e.OpenState(ctx, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	fin := ProbeFinalize{}
	if os.Getenv("APPCLONE_COMPAT_REPROBE") == "1" {
		fin.Existing, err = e.LoadProfile(app)
		if err != nil {
			t.Fatal(err)
		}
		fin.HasExisting = true
	}
	hints := e.StaticHints(ctx, d)
	public, err := e.probePublicBinds()
	if err != nil {
		t.Fatal(err)
	}
	ignored := e.probeSharedWrites()
	for _, bind := range public {
		ignored = append(ignored, bind.Src)
	}
	home, err := owned.OpenRoot("home")
	if err != nil {
		t.Fatal(err)
	}
	defer home.Close()
	if err := checkedRootPath(home, e.Resolver.Home); err != nil {
		t.Fatal(err)
	}
	before := compatHomeMetadata(t, home, e.Resolver.Home, ignored)
	control := &ProbeControl{}
	capture := &probeCaptureRunner{}
	var controlErr error
	var lastToken uint64
	finished := false
	res, p, _, _, probeErr := e.ProbeWithRetry(ctx, app, ProbeOptions{
		Command:  ProbeCommand(d.Exec, d.ExecArgs, fin.Existing, fin.HasExisting),
		LockFile: hints.LockFiles, StaticPaths: hints.Paths,
		Control: control, Runner: capture, Stderr: os.Stderr,
		Progress: func(progress ProbeProgress) {
			if progress.CanFinish && progress.SessionToken != lastToken {
				lastToken = progress.SessionToken
				fmt.Printf("COMPAT round=%d token=%d supervisor=%d finish=%s\n", progress.Round,
					lastToken, os.Getpid(), filepath.Join(controlPath, "finish-"+strconv.FormatUint(lastToken, 10)))
			}
			if !progress.CanFinish || controlErr != nil {
				return
			}
			name := "finish-" + strconv.FormatUint(progress.SessionToken, 10)
			st, statErr := controlDir.Lstat(name)
			if errors.Is(statErr, os.ErrNotExist) {
				return
			}
			if statErr != nil || !st.Mode().IsRegular() || st.Size() != 0 {
				controlErr = fmt.Errorf("invalid current finish request: %s", name)
				cancel()
				return
			}
			if err := controlDir.Remove(name); err != nil {
				controlErr = err
				cancel()
				return
			}
			finished = control.Finish(progress.SessionToken) || finished
		},
	}, fin)
	for _, process := range capture.processes {
		if !sessionGroupGone(process.PGID) {
			t.Fatalf("test session group not reclaimed: %d; retain %s", process.PGID, root)
		}
	}
	if controlErr != nil {
		t.Fatal(controlErr)
	}
	if probeErr != nil {
		t.Fatalf("probe: %v; cause: %v; retain %s", probeErr, errors.Unwrap(probeErr), root)
	}
	for path, original := range fixed {
		compatDirectory(t, owned, root, path)
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(original, current) {
			t.Fatalf("fixture directory changed: %s", path)
		}
	}
	if err := checkedRootPath(home, e.Resolver.Home); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(before, compatHomeMetadata(t, home, e.Resolver.Home, ignored)) {
		t.Fatal("probe changed host fixture HOME outside public/shared paths; retain fixture")
	}
	if _, err := home.Stat("Downloads/compat-public.txt"); err != nil {
		t.Fatal("public sentinel disappeared")
	}
	if !res.Started || len(p.Paths) == 0 || res.ReadTrace == "" {
		t.Fatal("missing startup/path/read-trace evidence")
	}
	if finished && (res.ReadTrace != ReadTracePartial && res.ReadTrace != ReadTraceUnavailable) {
		t.Fatal("manual finish overstated read completeness")
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	resultName := "result.json"
	if fin.HasExisting {
		resultName = "reprobe-result.json"
	}
	if err := controlDir.WriteFile(resultName, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("result=%s", filepath.Join(controlPath, resultName))
	if mounted, err := checkedMountUnder(e.TmpRoot); err != nil || mounted {
		t.Fatalf("probe mount remains: mounted=%v err=%v", mounted, err)
	}
	t.Logf("app=%s method=%s paths=%q writes=%d home_reads=%d read_trace=%s manual_finish=%v reclaimed=%d",
		app, p.ProbeMethod, p.Paths, res.Writes, res.HomeReads, res.ReadTrace, finished, len(capture.processes))
}

func compatDirectory(t *testing.T, owned *os.Root, root, path string) {
	t.Helper()
	if err := compatCheckDirectory(owned, root, path); err != nil {
		t.Fatal(err)
	}
}

func compatCheckDirectory(owned *os.Root, root, path string) error {
	if err := checkedRootPath(owned, root); err != nil {
		return err
	}
	table, err := checkedMountTable()
	if err != nil {
		return err
	}
	if err := checkOrdinaryMounts(table, path); err != nil {
		return err
	}
	for point := range table {
		if IsUnder(root, point) && IsUnder(point, path) {
			return fmt.Errorf("fixed fixture component is mounted: %s", point)
		}
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return errors.New("fixture component outside root")
	}
	current := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := owned.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture component must be an ordinary directory: %s", current)
		}
		atPath, err := os.Lstat(filepath.Join(root, current))
		if err != nil || !os.SameFile(info, atPath) {
			return fmt.Errorf("fixture component identity changed: %s", current)
		}
	}
	if Real(path) != path {
		return errors.New("fixture path no longer resolves to its original location")
	}
	return nil
}

func TestCompatFixtureIdentity(t *testing.T) {
	for _, replace := range []string{"root", "home", "ancestor"} {
		t.Run(replace, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "parent", "fixture")
			path := filepath.Join(root, "home", "nested")
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			owned, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer owned.Close()
			home, err := owned.OpenRoot("home")
			if err != nil {
				t.Fatal(err)
			}
			defer home.Close()
			if err := compatCheckDirectory(owned, root, path); err != nil {
				t.Fatal(err)
			}
			if err := checkedRootPath(home, filepath.Join(root, "home")); err != nil {
				t.Fatal(err)
			}
			target := root
			if replace == "home" {
				target = filepath.Join(root, "home")
			} else if replace == "ancestor" {
				target = filepath.Dir(root)
			}
			if err := os.Rename(target, target+"-old"); err != nil {
				t.Fatal(err)
			}
			// Old and replacement trees remain under t.TempDir.
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if replace != "home" && compatCheckDirectory(owned, root, path) == nil {
				t.Fatal("replaced root/ancestor accepted")
			}
			if checkedRootPath(home, filepath.Join(root, "home")) == nil {
				t.Fatal("replaced HOME accepted by retained handle")
			}
		})
	}
}

// Metadata only: reading does not disclose application file contents. Directory
// timestamps are excluded because a permitted shared child can change them.
func compatHomeMetadata(t *testing.T, home *os.Root, path string, ignored []string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := fs.WalkDir(home.FS(), ".", func(relative string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, allowed := range ignored {
			if IsUnder(allowed, filepath.Join(path, relative)) {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		info, err := home.Lstat(relative)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("file identity unavailable")
		}
		value := fmt.Sprintf("%d:%d:%v", stat.Dev, stat.Ino, info.Mode())
		if !info.IsDir() {
			value += fmt.Sprintf(":%d:%d", info.Size(), info.ModTime().UnixNano())
		}
		result[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
