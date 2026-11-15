package core

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Hooks inject faults in synthetic tests; production uses confined I/O.
var moveRename func(string, string) error
var removeChmod func(string, os.FileMode) error
var moveTestHook func(string)

func renameDirNoReplace(source *overlayScene, dst *copyDestination) error {
	if err := source.checkUnmounted(); err != nil {
		return err
	}
	if err := dst.check(); err != nil {
		return err
	}
	if moveRename != nil {
		return moveRename(source.path, dst.path)
	}
	af, err := source.parent.Open(".")
	if err != nil {
		return err
	}
	defer af.Close()
	bf, err := dst.parent.Open(".")
	if err != nil {
		return err
	}
	defer bf.Close()
	return unix.Renameat2(int(af.Fd()), source.name, int(bf.Fd()), dst.name, unix.RENAME_NOREPLACE)
}

type copyDestination struct {
	lexical, path, parentPath, name string
	parent                          *os.Root
	ancestors                       map[string]string
}

func openCopyDestination(path string) (*copyDestination, error) {
	table, err := checkedMountTable()
	if err != nil {
		return nil, sceneError(path, err)
	}
	canonical, err := ordinaryPath(table, path, false)
	if err != nil || table[path] != "" || table[canonical] != "" {
		return nil, sceneError(path, errors.New("invalid evidence destination"))
	}
	d := &copyDestination{lexical: path, path: canonical, parentPath: filepath.Dir(canonical), name: filepath.Base(canonical)}
	d.ancestors = mountAncestry(table, path, canonical)
	d.parent, err = openForkRoot(d.parentPath)
	if err != nil {
		return nil, err
	}
	if err := d.check(); err != nil {
		d.parent.Close()
		return nil, err
	}
	return d, nil
}

func (d *copyDestination) check() error {
	table, err := checkedMountTable()
	if err != nil {
		return sceneError(d.lexical, err)
	}
	if !maps.Equal(d.ancestors, mountAncestry(table, d.lexical, d.path)) {
		return sceneError(d.lexical, errors.New("destination ancestor mount changed"))
	}
	for point := range table {
		if IsUnder(d.path, point) {
			return sceneError(d.lexical, errors.New("destination contains a mount"))
		}
	}
	canonical, err := ordinaryPath(table, d.lexical, false)
	if err != nil || canonical != d.path {
		return sceneError(d.lexical, errors.New("destination alias changed"))
	}
	if err := checkedRootPath(d.parent, d.parentPath); err != nil {
		return sceneError(d.lexical, err)
	}
	return nil
}

func (d *copyDestination) create() (*overlayScene, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	if err := d.parent.Mkdir(d.name, 0o700); err != nil {
		return nil, err
	}
	created, err := d.parent.Lstat(d.name)
	if err != nil {
		return nil, err
	}
	if err := d.check(); err != nil {
		return nil, err
	}
	scene, err := openOverlayScene(d.lexical)
	if err != nil {
		return nil, err
	}
	if scene.info == nil || !os.SameFile(created, scene.info) {
		scene.close()
		return nil, sceneError(d.lexical, errors.New("new destination changed"))
	}
	return scene, nil
}

func moveDir(src, dst string) error {
	scene, err := openOverlayScene(src)
	if err != nil {
		return err
	}
	defer scene.close()
	return moveSceneDir(scene, scene, dst)
}

// scene covers the entire overlay tree even when only upper is moved.
// Safety failures cannot be turned into a successful evidence move.
func moveSceneDir(scene, source *overlayScene, dst string) error {
	if err := scene.checkUnmounted(); err != nil {
		return err
	}
	if source.absent {
		return os.ErrNotExist
	}
	if !IsUnder(scene.path, source.path) {
		return sceneError(source.lexical, errors.New("source is outside its scene"))
	}
	if err := source.checkUnmounted(); err != nil {
		return err
	}
	destination, err := openCopyDestination(dst)
	if err != nil {
		return err
	}
	defer destination.parent.Close()
	if _, err := destination.parent.Lstat(destination.name); err == nil {
		return fmt.Errorf("moveDir: destination exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := scene.checkUnmounted(); err != nil {
		return err
	}
	if err := renameDirNoReplace(source, destination); err == nil {
		source.absent = true
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := scene.checkUnmounted(); err != nil {
		return err
	}
	if err := source.checkUnmounted(); err != nil {
		return err
	}
	target, err := destination.create()
	if err != nil {
		return err
	}
	defer target.close()
	check := func() error {
		if err := scene.checkUnmounted(); err != nil {
			return err
		}
		if err := source.checkUnmounted(); err != nil {
			return err
		}
		return target.checkUnmounted()
	}
	discardTarget := func(cause error) error {
		if err := check(); err != nil {
			return err
		}
		if err := target.remove("."); err != nil {
			return err
		}
		return cause
	}
	if err := check(); err != nil {
		return discardTarget(err)
	}
	if err := copyRootTree(source.root, target.root); err != nil {
		if moveTestHook != nil {
			moveTestHook("copy-failed")
		}
		return discardTarget(err)
	}
	if err := syncRootTree(target.root); err != nil {
		return discardTarget(err)
	}
	if moveTestHook != nil {
		moveTestHook("after-copy")
	}
	if err := check(); err != nil {
		return err
	}
	if err := source.remove("."); err != nil {
		if checkErr := check(); checkErr != nil {
			return checkErr
		}
		if werr := makeSceneWritableAndRemove(scene, source); werr == nil {
			return nil
		}
		if checkErr := check(); checkErr != nil {
			return checkErr
		}
		return &dstCompleteError{dst: dst, err: err}
	}
	return nil
}

func createCopyScene(dst string) (*overlayScene, error) {
	destination, err := openCopyDestination(dst)
	if err != nil {
		return nil, err
	}
	defer destination.parent.Close()
	return destination.create()
}

func makeSceneWritableAndRemove(scene, source *overlayScene) error {
	if err := scene.checkUnmounted(); err != nil {
		return err
	}
	if err := source.checkUnmounted(); err != nil {
		return err
	}
	if source.absent {
		return nil
	}
	_ = fs.WalkDir(source.root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if removeChmod != nil {
				if err := removeChmod(filepath.Join(source.path, path), 0o700); err != nil {
					return nil
				}
			}
			_ = source.root.Chmod(path, 0o700)
		}
		return nil
	})
	if err := scene.checkUnmounted(); err != nil {
		return err
	}
	return source.remove(".")
}

type dstCompleteError struct {
	dst string
	err error
}

func (e *dstCompleteError) Error() string {
	return fmt.Sprintf("moveDir: destination complete (%s), source not fully removed: %v", e.dst, e.err)
}
func (e *dstCompleteError) Unwrap() error { return e.err }
func dstComplete(err error) bool          { var dc *dstCompleteError; return errors.As(err, &dc) }

// Standalone copy leaves source removal to its caller.
func copyTree(src, dst string) error {
	source, err := openOverlayScene(src)
	if err != nil {
		return err
	}
	defer source.close()
	if source.absent {
		return os.ErrNotExist
	}
	if err := source.checkUnmounted(); err != nil {
		return err
	}
	target, err := createCopyScene(dst)
	if err != nil {
		return err
	}
	defer target.close()
	if err := target.checkUnmounted(); err != nil {
		return err
	}
	return copyRootTree(source.root, target.root)
}

func copyRootTree(src, dst *os.Root) error {
	return fs.WalkDir(src.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := forkNoSymlinks(src, filepath.Dir(path)); err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return forkMkdirAll(dst, path)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := src.Readlink(path)
			if err != nil {
				return err
			}
			return dst.Symlink(link, path)
		case d.Type().IsRegular():
			return copyRootFile(src, dst, path)
		default:
			return nil // Whiteouts/devices/FIFOs/socket entries carry no file content.
		}
	})
}

func copyRootFile(src, dst *os.Root, path string) error {
	in, err := src.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("evidence file changed type")
	}
	out, err := dst.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

func syncRootTree(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		if err := forkNoSymlinks(root, path); err != nil {
			return err
		}
		return forkSyncDir(root, path)
	})
}

// Permission/read errors do not establish that upper is absent.
func removeAuditScene(scene *overlayScene) error {
	if err := scene.checkUnmounted(); err != nil {
		return err
	}
	if scene.absent {
		return nil
	}
	if _, err := scene.root.Lstat("upper"); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return sceneError(scene.path, err)
	}
	return scene.remove(".")
}
