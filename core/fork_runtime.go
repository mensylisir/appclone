package core

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A descriptor and the path used by mountinfo must identify the same tree.
func checkedRootPath(root *os.Root, path string) error {
	canonical, err := checkedOrdinaryPath(path, false)
	if err != nil || canonical != path {
		return errors.New("tool directory path changed")
	}
	want, err := root.Stat(".")
	if err != nil {
		return err
	}
	got, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !got.IsDir() || !os.SameFile(want, got) {
		return errors.New("tool directory identity changed")
	}
	return nil
}

func openForkChild(root *os.Root, rel string) (*os.Root, error) {
	if err := forkNoSymlinks(root, rel); err != nil {
		return nil, err
	}
	want, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	r, err := root.OpenRoot(rel)
	if err != nil {
		return nil, err
	}
	got, err := r.Stat(".")
	if err != nil || !os.SameFile(want, got) {
		r.Close()
		return nil, errors.New("snapshot child changed while opening")
	}
	return r, nil
}

// Remember ordinary ancestors and exact mount records. Never stat a known FUSE
// mount: a disconnected daemon could make that stat block indefinitely.
type forkRuntimeGuard struct {
	root     *os.Root
	path     string
	files    map[string]os.FileInfo
	mounts   map[string]string
	retain   error
	identity *forkRunIdentity
}

func newForkRuntimeGuard(root *os.Root, path string) *forkRuntimeGuard {
	return &forkRuntimeGuard{root: root, path: path, files: map[string]os.FileInfo{}, mounts: map[string]string{}}
}

func (g *forkRuntimeGuard) remember(rel string) error {
	if err := g.identity.check(); err != nil {
		return err
	}
	table, err := checkedMountTable()
	if err != nil {
		return err
	}
	if err := checkForkStarting(g.root, g.path, table); err != nil {
		return err
	}
	if err := checkedRootPath(g.root, g.path); err != nil {
		return err
	}
	for p := rel; p != "."; p = filepath.Dir(p) {
		if err := checkOrdinaryMounts(table, filepath.Join(g.path, p)); err != nil {
			return err
		}
		if table[filepath.Join(g.path, p)] != "" {
			return errors.New("ordinary bind source is mounted")
		}
		fi, err := g.root.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("bind source contains symbolic link")
		}
		if old, exists := g.files[p]; exists && !os.SameFile(old, fi) {
			return errors.New("bind ancestor changed")
		}
		g.files[p] = fi
	}
	return nil
}

func (g *forkRuntimeGuard) mounted(rel string) error {
	// The caller recorded ancestors before starting the mount. Check those
	// without touching the mountpoint whose identity has just changed.
	delete(g.files, rel)
	table, err := checkedMountTable()
	if err != nil {
		return err
	}
	path := filepath.Join(g.path, rel)
	if record, ok := table[path]; ok {
		g.mounts[path] = record
	} else {
		return errors.New("successful fork mount has no trusted mount record")
	}
	return g.check()
}

func (g *forkRuntimeGuard) check() error {
	if g.retain != nil {
		return g.retain
	}
	if err := g.identity.check(); err != nil {
		return err
	}
	table, err := checkedMountTable()
	if err != nil {
		return err
	}
	if err := checkForkStarting(g.root, g.path, table); err != nil {
		return err
	}
	if err := checkedRootPath(g.root, g.path); err != nil {
		return err
	}
	for path, record := range g.mounts {
		if table[path] != record {
			return errors.New("fork mount identity changed")
		}
	}
	for path := range table {
		if IsUnder(filepath.Join(g.path, ".fork"), path) && g.mounts[path] == "" {
			return errors.New("unexpected fork mount")
		}
	}
	for rel, want := range g.files {
		if err := checkOrdinaryMounts(table, filepath.Join(g.path, rel)); err != nil {
			return err
		}
		if table[filepath.Join(g.path, rel)] != "" {
			return errors.New("ordinary bind source is mounted")
		}
		got, err := g.root.Lstat(rel)
		if err != nil {
			return err
		}
		if got.Mode()&os.ModeSymlink != 0 || want == nil || !os.SameFile(want, got) {
			return errors.New("fork bind source identity changed")
		}
	}
	return nil
}

// A crash or failed Mount must remain distinguishable from a mount whose
// identity was confirmed. Reset only clears this marker after all mounts vanish.
func (g *forkRuntimeGuard) beginMount() error {
	if err := g.check(); err != nil {
		return err
	}
	f, err := g.root.OpenFile(".fork/mount-pending", os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return forkSyncDir(g.root, ".fork")
}

func (g *forkRuntimeGuard) confirmMount() error {
	if err := g.check(); err != nil {
		return err
	}
	if err := g.root.Remove(".fork/mount-pending"); err != nil {
		return err
	}
	return forkSyncDir(g.root, ".fork")
}

// Every upper, including non-inherited files and empty directories, is created
// through a confined handle. Existing files are never truncated.
func prepareForkUpper(root *os.Root, rel, host string) error {
	if err := forkMkdirAll(root, filepath.Dir(rel)); err != nil {
		return err
	}
	hostInfo, hostErr := os.Stat(host)
	if hostErr != nil && !os.IsNotExist(hostErr) {
		return hostErr
	}
	fi, err := root.Lstat(rel)
	if err == nil {
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return errors.New("upper is not an ordinary directory or file")
		}
		if hostErr == nil && hostInfo.IsDir() != fi.IsDir() {
			return errors.New("host and upper types differ")
		}
		return forkNoSymlinks(root, rel)
	}
	if !os.IsNotExist(err) {
		return err
	}
	if hostErr == nil && !hostInfo.IsDir() {
		if !hostInfo.Mode().IsRegular() {
			return errors.New("host is not an ordinary file")
		}
		f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
		if err != nil {
			return err
		}
		return f.Close()
	}
	return forkMkdirAll(root, rel)
}

func checkForkHashes(paths []string) error {
	seen := map[string]string{}
	for _, path := range paths {
		h := forkHash(path)
		if old := seen[h]; old != "" && old != path {
			return errors.New("isolated data paths have conflicting mount names")
		}
		seen[h] = path
	}
	return nil
}
