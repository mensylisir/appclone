package core

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The upper is ordinary storage, even while its sibling merged is mounted.
// Its identity is retained by the scene through every read and later disposal.
func (s *overlayScene) captureUpper() error {
	if err := s.checkIdle(); err != nil {
		return err
	}
	if s.upper == nil {
		upper, err := openOverlayScene(filepath.Join(s.lexical, "upper"))
		if err != nil {
			s.upperErr = err
			return err
		}
		s.upper = upper
	}
	return s.checkIdle()
}

func (s *overlayScene) readUpper() ([]UpperEntry, bool, error) {
	if err := s.captureUpper(); err != nil {
		return nil, false, err
	}
	if s.absent || s.upper.absent {
		return nil, false, nil
	}
	entries, err := readUpperRoot(s.upper.root)
	if checkErr := s.checkIdle(); checkErr != nil {
		return nil, false, checkErr
	}
	if err != nil {
		return nil, false, errEnv("err.env.read_dir", map[string]any{"path": s.upper.lexical}, err)
	}
	return entries, true, nil
}

func readUpperRoot(root *os.Root) ([]UpperEntry, error) {
	var entries []UpperEntry
	err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." || d.Name() == ".wh..wh..opq" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := UpperEntry{Path: filepath.ToSlash(path), Kind: EntryFile, Size: info.Size()}
		switch {
		case info.IsDir():
			entry.Kind = EntryDir
		case strings.HasPrefix(d.Name(), ".wh."):
			entry.Kind = EntryWhiteout
			entry.Path = filepath.ToSlash(filepath.Join(filepath.Dir(path), strings.TrimPrefix(d.Name(), ".wh.")))
		case info.Mode()&fs.ModeDevice != 0 && info.Mode()&fs.ModeCharDevice != 0:
			entry.Kind = EntryWhiteout
		case !info.Mode().IsRegular():
			entry.Kind = EntryOther
		}
		entries = append(entries, entry)
		return nil
	})
	return entries, err
}
