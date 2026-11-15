package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Control paths must live on a known local kernel filesystem. In particular,
// checking a dead FUSE path with Stat can block before any timeout is started.
func checkOrdinaryMounts(table map[string]string, path string) error {
	for point, records := range table {
		if !IsUnder(point, path) {
			continue
		}
		for _, record := range strings.Split(strings.TrimSpace(records), "\n") {
			_, after, ok := strings.Cut(record, " - ")
			if !ok {
				return errors.New("invalid mount record")
			}
			kind, _, _ := strings.Cut(after, " ")
			switch kind {
			case "ext2", "ext3", "ext4", "btrfs", "xfs", "f2fs", "tmpfs", "ramfs", "rootfs", "overlay", "zfs", "bcachefs":
			default:
				return errors.New("control path is covered by an unsupported mount")
			}
		}
	}
	return nil
}

// Resolve each link only after inspecting its lexical target in mountinfo.
// resolveFinal=false leaves the basename untouched, so a scene-root link is
// rejected by its caller rather than silently accepted as another directory.
func ordinaryPath(table map[string]string, path string, resolveFinal bool) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", errors.New("control path is not absolute")
	}
	if err := checkOrdinaryMounts(table, path); err != nil {
		return "", err
	}
	pending := strings.Split(strings.TrimPrefix(path, "/"), "/")
	resolved := "/"
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		if err := checkOrdinaryMounts(table, next); err != nil {
			return "", err
		}
		if len(pending) == 0 && !resolveFinal {
			return next, nil
		}
		fi, err := os.Lstat(next)
		if os.IsNotExist(err) {
			full := filepath.Join(append([]string{next}, pending...)...)
			if err := checkOrdinaryMounts(table, full); err != nil {
				return "", err
			}
			return full, nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		links++
		if links > 40 {
			return "", errors.New("too many control path symbolic links")
		}
		link, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(link) {
			resolved = "/"
		}
		pending = append(strings.Split(link, "/"), pending...)
	}
	return resolved, nil
}

func checkedOrdinaryPath(path string, resolveFinal bool) (string, error) {
	table, err := checkedMountTable()
	if err != nil {
		return "", sceneError(path, err)
	}
	canonical, err := ordinaryPath(table, path, resolveFinal)
	if err != nil {
		return "", sceneError(path, err)
	}
	return canonical, nil
}

func checkControlMount(table map[string]string, path string) error {
	if err := checkOrdinaryMounts(table, path); err != nil {
		return err
	}
	for point := range table {
		if IsUnder(path, point) {
			return errors.New("control file or directory is mounted")
		}
	}
	return nil
}
