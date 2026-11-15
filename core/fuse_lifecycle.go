package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const fuseStartingName = ".fuse-starting.json"

type fuseStartingRecord struct {
	Version int    `json:"version"`
	BootID  string `json:"boot_id"`
	Nonce   string `json:"nonce"`
}

func readFuseStarting(root *os.Root, path string, table map[string]string) (fuseStartingRecord, os.FileInfo, error) {
	var rec fuseStartingRecord
	if err := checkControlMount(table, filepath.Join(path, fuseStartingName)); err != nil {
		return rec, nil, err
	}
	f, err := root.OpenFile(fuseStartingName, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return rec, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return rec, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return rec, info, errors.New("invalid FUSE startup marker")
	}
	dec := json.NewDecoder(io.LimitReader(f, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&rec) != nil || dec.Decode(new(any)) != io.EOF || rec.Version != 1 || !validProbeBootID(rec.BootID) {
		return rec, info, errors.New("invalid FUSE startup record")
	}
	nonce, err := hex.DecodeString(rec.Nonce)
	if err != nil || len(nonce) != 12 {
		return rec, info, errors.New("invalid FUSE startup identity")
	}
	return rec, info, nil
}

func checkFuseStarting(root *os.Root, path string, table map[string]string) error {
	rec, _, err := readFuseStarting(root, path, table)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	boot, err := readProbeBootID()
	if err != nil {
		return err
	}
	if rec.BootID == boot {
		return errors.New("FUSE startup or shutdown has not been confirmed complete; restart if this persists")
	}
	return nil
}

type fuseStarting struct {
	scene  *overlayScene
	record fuseStartingRecord
	info   os.FileInfo
}

func beginFuseStarting(scene *overlayScene) (*fuseStarting, error) {
	if err := scene.checkIdle(); err != nil {
		return nil, err
	}
	if scene.root == nil {
		return nil, sceneError(scene.path, errors.New("missing FUSE work directory"))
	}
	boot, err := readProbeBootID()
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	record := fuseStartingRecord{Version: 1, BootID: boot, Nonce: hex.EncodeToString(nonce[:])}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	f, err := scene.root.OpenFile(fuseStartingName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := forkSyncDir(scene.root, "."); err != nil {
		return nil, err
	}
	if _, err := scene.checkStructure(); err != nil {
		return nil, err
	}
	return &fuseStarting{scene: scene, record: record, info: info}, nil
}

// Only the driver which created this marker may remove it. Generic cleanup
// never treats a same-boot marker as permission to stop or delete anything.
func (m *fuseStarting) complete() error {
	table, err := m.scene.checkStructure()
	if err != nil {
		return err
	}
	record, info, err := readFuseStarting(m.scene.root, m.scene.path, table)
	if err != nil {
		return err
	}
	if record != m.record || !os.SameFile(m.info, info) {
		return errors.New("FUSE startup marker changed")
	}
	if err := m.scene.root.Remove(fuseStartingName); err != nil {
		return err
	}
	return forkSyncDir(m.scene.root, ".")
}

// Inspect only ordinary .fork and one level of mount work directories. Never
// walk merged: inspecting a stale FUSE mount could itself wait indefinitely.
func checkForkStarting(inst *os.Root, instPath string, table map[string]string) error {
	base := filepath.Join(instPath, ".fork")
	if err := checkOrdinaryMounts(table, base); err != nil {
		return err
	}
	if table[instPath] != "" || table[base] != "" {
		return errors.New("fork control directory is mounted")
	}
	for point := range table {
		if filepath.Dir(point) == base {
			return errors.New("fork work directory is mounted")
		}
	}
	fi, err := inst.Lstat(".fork")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("fork work root is not an ordinary directory")
	}
	root, err := openForkChild(inst, ".fork")
	if err != nil {
		return err
	}
	defer root.Close()
	if err := checkFuseStarting(root, base, table); err != nil {
		return err
	}
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("fork control directory contains a symbolic link")
		}
		if !entry.IsDir() {
			continue
		}
		childPath := filepath.Join(base, entry.Name())
		if err := checkOrdinaryMounts(table, childPath); err != nil {
			return err
		}
		child, err := openForkChild(root, entry.Name())
		if err != nil {
			return err
		}
		err = checkFuseStarting(child, childPath, table)
		child.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
