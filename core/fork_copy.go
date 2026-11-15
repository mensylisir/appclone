package core

// Fixed fork sources are copied through confined directory handles. Complete
// metadata inventories detect changes across the copy, including same-size
// rewrites and replaced ancestors; this is not a database transaction snapshot.
import (
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type forkNode struct {
	Mode         os.FileMode
	Dev, Ino     uint64
	Size         int64
	MTime, CTime syscall.Timespec
	Link         string
}

func forkIdentity(fi os.FileInfo) forkNode {
	s := fi.Sys().(*syscall.Stat_t)
	return forkNode{Mode: fi.Mode(), Dev: uint64(s.Dev), Ino: s.Ino, Size: fi.Size(), MTime: s.Mtim, CTime: s.Ctim}
}

func forkStopped(stop func() bool) error {
	if stop != nil && stop() {
		return ErrCanceled
	}
	return nil
}

// The path is normalized before opening; verify the descriptor refers to the
// checked root. Descendants are always accessed relative to this descriptor.
func openForkRoot(path string) (*os.Root, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("snapshot root is not a directory")
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(fi, opened) {
		r.Close()
		return nil, errors.New("snapshot root changed while opening")
	}
	return r, nil
}

func forkInventory(r *os.Root, stop func() bool) (map[string]forkNode, error) {
	out := map[string]forkNode{}
	err := fs.WalkDir(r.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := forkStopped(stop); err != nil {
			return err
		}
		fi, err := r.Lstat(path)
		if err != nil {
			return err
		}
		n := forkIdentity(fi)
		if fi.Mode()&os.ModeSymlink != 0 {
			n.Link, err = r.Readlink(path)
			if err != nil {
				return err
			}
		} else if !fi.IsDir() && !fi.Mode().IsRegular() {
			// Sockets/locks must not be opened or silently omitted from a copy.
			return errors.New("source contains a special file; close the main application and retry")
		}
		out[path] = n
		return nil
	})
	return out, err
}

func forkSyncDir(r *os.Root, path string) error {
	f, err := r.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// clone is injectable per copy for filesystems without reflink and failure tests.
func copyForkFile(src, dst *os.Root, path string, want forkNode, stop func() bool, clone func(int, int) error) error {
	if err := forkStopped(stop); err != nil {
		return err
	}
	in, err := src.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if forkIdentity(fi) != want || !fi.Mode().IsRegular() {
		return errors.New("snapshot source changed")
	}
	out, err := dst.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	err = clone(int(out.Fd()), int(in.Fd()))
	if err != nil {
		if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.ENOTTY) && !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) {
			return err
		}
		buf := make([]byte, 256*1024)
		for {
			if err := forkStopped(stop); err != nil {
				return err
			}
			n, re := in.Read(buf)
			if n > 0 {
				if _, err := out.Write(buf[:n]); err != nil {
					return err
				}
			}
			if re == io.EOF {
				break
			}
			if re != nil {
				return re
			}
		}
	}
	fi, err = in.Stat()
	if err != nil {
		return err
	}
	if forkIdentity(fi) != want {
		return errors.New("snapshot source changed during copy")
	}
	if err := forkStopped(stop); err != nil {
		return err
	}
	// Preserve owner executable/read-only semantics without copying group access.
	if err := out.Chmod(want.Mode.Perm() & 0o700); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

func copyForkTree(src, dst *os.Root, before map[string]forkNode, stop func() bool, clone func(int, int) error) error {
	err := fs.WalkDir(src.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := forkStopped(stop); err != nil {
			return err
		}
		want, ok := before[path]
		if !ok {
			return errors.New("snapshot source gained an entry")
		}
		if path == "." {
			return nil
		}
		switch {
		case want.Mode.IsDir():
			return dst.Mkdir(path, 0o700)
		case want.Mode&os.ModeSymlink != 0:
			// Keep the link itself. Never read/copy its target.
			return dst.Symlink(want.Link, path)
		case want.Mode.IsRegular():
			return copyForkFile(src, dst, path, want, stop, clone)
		default:
			return errors.New("unsupported snapshot entry")
		}
	})
	if err != nil {
		return err
	}
	after, err := forkInventory(src, stop)
	if err != nil {
		return err
	}
	if !maps.Equal(before, after) {
		return errors.New("snapshot source changed during copy")
	}
	// All directories must be durable before publishing their generation.
	for path, n := range before {
		if n.Mode.IsDir() {
			if err := forkSyncDir(dst, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// No symlink components within a tool-owned snapshot tree. os.Root supplies
// confinement under races; this also rejects aliases within that tree.
func forkNoSymlinks(r *os.Root, path string) error {
	for p := filepath.Clean(path); p != "."; p = filepath.Dir(p) {
		fi, err := r.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("snapshot path is a symbolic link")
		}
	}
	return nil
}
