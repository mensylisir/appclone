package core

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
)

var sceneTestHook func(stage, path string) // nil outside synthetic fault tests

// An overlay scene owns the whole temporary tree, including merged and the
// driver marker. Evidence operations on upper must keep this larger boundary.
type overlayScene struct {
	lexical, path, parentPath, name string
	parent, root                    *os.Root
	info                            os.FileInfo
	ancestors                       map[string]string
	absent                          bool // Originally missing, or removed by this handle.
	active                          *probeActiveState
	activeKnown                     bool
	activeErr                       error
	upper                           *overlayScene // Captured once; never recaptured after drift.
	upperErr                        error
}

func sceneError(path string, cause error) error {
	return errEnv("err.probe.scene_unsafe", map[string]any{"path": path}, cause)
}

func mountCoversDirectory(table map[string]string, dir string) bool {
	for point := range table {
		if point == dir {
			return true
		}
	}
	return false
}

func mountAncestry(table map[string]string, paths ...string) map[string]string {
	out := map[string]string{}
	for point, record := range table {
		for _, path := range paths {
			if IsUnder(point, filepath.Dir(path)) {
				out[point] = record
			}
		}
	}
	return out
}

func openOverlayScene(path string) (*overlayScene, error) {
	// Never open a scene root which is itself a mount, including disconnected
	// FUSE. The table must be readable even when the directory seems absent.
	table, err := checkedMountTable()
	if err != nil {
		return nil, sceneError(path, err)
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || path == "/" {
		return nil, sceneError(path, errors.New("invalid temporary tree"))
	}
	if mountCoversDirectory(table, path) {
		return nil, sceneError(path, errors.New("scene root is mounted"))
	}
	canonical, err := ordinaryPath(table, path, false)
	if err != nil {
		return nil, sceneError(path, err)
	}
	if mountCoversDirectory(table, canonical) {
		return nil, sceneError(path, errors.New("scene root is mounted"))
	}
	s := &overlayScene{lexical: path, path: canonical, parentPath: filepath.Dir(canonical), name: filepath.Base(canonical)}
	s.ancestors = mountAncestry(table, path, canonical)
	fi, err := os.Lstat(canonical)
	if os.IsNotExist(err) {
		s.absent = true
		for point := range table {
			if IsUnder(canonical, point) {
				return nil, sceneError(path, errors.New("missing scene still has mounts"))
			}
		}
		// A missing descendant still belongs to one particular existing parent.
		// Keep that parent's handle even when several path components are absent.
		for {
			parentInfo, perr := os.Lstat(s.parentPath)
			if perr == nil {
				if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
					return nil, sceneError(path, errors.New("missing scene parent is not a directory"))
				}
				break
			}
			if !os.IsNotExist(perr) || s.parentPath == "/" {
				return nil, sceneError(path, perr)
			}
			s.parentPath = filepath.Dir(s.parentPath)
		}
		s.name, err = filepath.Rel(s.parentPath, s.path)
		if err != nil || !filepath.IsLocal(s.name) {
			return nil, sceneError(path, errors.New("invalid missing scene location"))
		}
		s.parent, err = openForkRoot(s.parentPath)
		if err != nil {
			return nil, sceneError(path, err)
		}
		if _, err := s.checkStructure(); err != nil {
			s.close()
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, sceneError(path, err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, sceneError(path, errors.New("scene is not an ordinary directory"))
	}
	s.info = fi
	s.parent, err = openForkRoot(s.parentPath)
	if err != nil {
		return nil, sceneError(path, err)
	}
	s.root, err = openForkChild(s.parent, s.name)
	if err != nil {
		s.close()
		return nil, sceneError(path, err)
	}
	if _, err := s.checkStructure(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *overlayScene) close() {
	if s == nil {
		return
	}
	if s.upper != nil {
		s.upper.close()
	}
	if s.root != nil {
		s.root.Close()
	}
	if s.parent != nil {
		s.parent.Close()
	}
}

func ownedOverlayScene(path string) (*overlayScene, error) {
	s, err := openOverlayScene(path)
	if err != nil {
		return nil, sceneError(path, err)
	}
	if s.info == nil || !fileOwnedBy(s.info, os.Getuid()) {
		s.close()
		return nil, nil
	}
	return s, nil
}

func (s *overlayScene) checkStructure() (map[string]string, error) {
	table, err := checkedMountTable()
	if err != nil {
		return nil, sceneError(s.lexical, err)
	}
	if !maps.Equal(s.ancestors, mountAncestry(table, s.lexical, s.path)) {
		return nil, sceneError(s.lexical, errors.New("scene ancestor mount changed"))
	}
	if mountCoversDirectory(table, s.path) || mountCoversDirectory(table, s.lexical) {
		return nil, sceneError(s.lexical, errors.New("scene root is mounted"))
	}
	canonical, err := ordinaryPath(table, s.lexical, false)
	if err != nil || canonical != s.path {
		return nil, sceneError(s.lexical, errors.New("scene alias changed"))
	}
	if s.parent != nil {
		if err := checkedRootPath(s.parent, s.parentPath); err != nil {
			return nil, sceneError(s.lexical, err)
		}
	}
	if s.absent {
		if _, err := s.parent.Lstat(s.name); !os.IsNotExist(err) {
			return nil, sceneError(s.lexical, errors.New("missing scene changed"))
		}
	} else {
		if err := checkedRootPath(s.root, s.path); err != nil {
			return nil, sceneError(s.lexical, err)
		}
		got, err := s.root.Stat(".")
		if err != nil || !os.SameFile(s.info, got) {
			return nil, sceneError(s.lexical, errors.New("scene identity changed"))
		}
	}
	return table, nil
}

func (s *overlayScene) checkIdle() error {
	table, err := s.checkStructure()
	if err != nil {
		return err
	}
	if s.absent {
		return nil
	}
	if err := checkFuseStarting(s.root, s.path, table); err != nil {
		return sceneError(s.lexical, err)
	}
	if s.activeKnown {
		if err := s.checkActiveIdentity(); err != nil {
			return err
		}
	}
	if s.upper != nil {
		if err := s.upper.checkUnmounted(); err != nil {
			return err
		}
	}
	if s.upperErr != nil {
		return s.upperErr
	}
	return nil
}

func (s *overlayScene) checkUnmounted() error {
	if err := s.checkIdle(); err != nil {
		return err
	}
	table, err := s.checkStructure()
	if err != nil {
		return err
	}
	for point := range table {
		if IsUnder(s.path, point) || IsUnder(s.lexical, point) {
			return sceneError(s.lexical, errors.New("temporary tree still contains a mount"))
		}
	}
	return nil
}

func (s *overlayScene) remove(rel string) error {
	if !filepath.IsLocal(rel) {
		return sceneError(s.lexical, errors.New("invalid scene child"))
	}
	if err := s.checkUnmounted(); err != nil {
		return err
	}
	if s.absent {
		return nil
	}
	if rel == "." {
		// Preserve the state of each successfully removed tracked child when a
		// later permission error leaves the rest of this scene in place.
		if s.upper != nil && !s.upper.absent {
			if err := s.upper.remove("."); err != nil {
				return err
			}
			if err := s.checkUnmounted(); err != nil {
				return err
			}
		}
		if s.activeKnown {
			if err := s.clearActive(); err != nil {
				return err
			}
			if err := s.checkUnmounted(); err != nil {
				return err
			}
		}
		if sceneTestHook != nil {
			sceneTestHook("after-scene-children", s.path)
		}
		if err := s.checkUnmounted(); err != nil {
			return err
		}
		if err := s.parent.RemoveAll(s.name); err != nil {
			return sceneError(s.lexical, err)
		}
		s.absent = true
		if s.upper != nil {
			s.upper.absent = true
		}
		return nil
	}
	if err := s.root.RemoveAll(rel); err != nil {
		return sceneError(s.lexical, err)
	}
	return nil
}

func removeOverlayScene(path string) error {
	s, err := openOverlayScene(path)
	if err != nil {
		return err
	}
	defer s.close()
	return s.remove(".")
}

// Stale cleanup only unmounts the known merged location. Unexpected sibling or
// descendant mounts keep the entire scene intact for diagnosis.
func (s *overlayScene) detachStale() error {
	if err := s.checkIdle(); err != nil {
		return err
	}
	table, err := s.checkStructure()
	if err != nil {
		return err
	}
	merged := filepath.Join(s.path, "merged")
	for point := range table {
		if IsUnder(s.path, point) && point != merged {
			return sceneError(s.lexical, errors.New("unexpected scene mount"))
		}
	}
	if record := table[merged]; record != "" {
		if err := s.checkIdle(); err != nil {
			return err
		}
		current, err := s.checkStructure()
		if err != nil {
			return err
		}
		if current[merged] != record {
			return sceneError(s.lexical, errors.New("scene mount changed"))
		}
		if err := umount(merged); err != nil {
			return sceneError(s.lexical, err)
		}
	}
	return s.checkUnmounted()
}
