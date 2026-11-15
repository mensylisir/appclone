package core

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The existing run.lock remains the sole running-state authority. Its optional
// contents only describe progress while held; old versions ignore the contents.
func writeRunPhase(lock *os.File, phase string) error {
	if err := checkRunPhaseFile(lock); err != nil {
		return err
	}
	if _, err := lock.WriteAt([]byte(phase+"\n"), 0); err != nil {
		return err
	}
	return lock.Truncate(int64(len(phase) + 1))
}

func checkRunPhaseFile(lock *os.File) error {
	fi, err := lock.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || Real(filepath.Dir(lock.Name())) != filepath.Dir(lock.Name()) {
		return errors.New("invalid run lock")
	}
	current, err := os.Lstat(lock.Name())
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(fi, current) {
		return errors.New("run lock identity changed")
	}
	return nil
}

// IsPreparing is a UI hint, never permission to start/stop/remove an instance.
func (e *Env) IsPreparing(app, id string) bool {
	running, err := e.IsRunning(app, id)
	if err != nil || !running {
		return false
	}
	f, err := os.OpenFile(filepath.Join(e.InstancePath(app, id), "run.lock"), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(f, 32))
	return err == nil && strings.TrimSpace(string(b)) == "preparing"
}
