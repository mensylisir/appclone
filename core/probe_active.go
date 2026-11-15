package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SessionProcess identifies a process without confusing a reused PID with the
// original session. PGID is checked by the runner before exposing the process.
type SessionProcess struct {
	PID   int
	Start uint64
	UID   int
	PGID  int
}

type probeActiveRecord struct {
	Version   int
	BootID    string
	Owner     SessionProcess
	Target    *SessionProcess
	Launching bool
}

var unreapedProbes sync.Map // work directory -> retained session

func probeSessionsBlocked() bool {
	blocked := false
	unreapedProbes.Range(func(_, _ any) bool { blocked = true; return false })
	return blocked
}

func identifySessionProcess(pid, pgid int) (SessionProcess, error) {
	start, err := procStartTime(pid)
	if err != nil {
		return SessionProcess{}, err
	}
	fi, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return SessionProcess{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return SessionProcess{}, errors.New("process owner mismatch")
	}
	return SessionProcess{PID: pid, Start: start, UID: int(st.Uid), PGID: pgid}, nil
}

type probeActiveState struct {
	info os.FileInfo
	data string
}

func (s *overlayScene) checkControlFile(name string) error {
	table, err := s.checkStructure()
	if err != nil {
		return err
	}
	if err := checkControlMount(table, filepath.Join(s.path, name)); err != nil {
		return sceneError(s.lexical, err)
	}
	if s.absent {
		return os.ErrNotExist
	}
	return nil
}

func (s *overlayScene) readActiveFile() (probeActiveRecord, os.FileInfo, string, error) {
	var rec probeActiveRecord
	if err := s.checkControlFile("active.json"); err != nil {
		return rec, nil, "", err
	}
	f, err := s.root.OpenFile("active.json", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return rec, nil, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return rec, nil, "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || fi.Size() > 4096 || !ok || int(st.Uid) != os.Getuid() {
		return rec, nil, "", errors.New("invalid active marker")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return rec, nil, "", errors.New("cannot read active marker")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if dec.Decode(&rec) != nil || rec.Version != 1 || dec.Decode(new(any)) != io.EOF {
		return rec, nil, "", errors.New("invalid active record")
	}
	return rec, fi, string(data), nil
}

func (s *overlayScene) checkActiveIdentity() (retErr error) {
	if s.activeErr != nil {
		return s.activeErr
	}
	defer func() {
		if retErr != nil {
			s.activeErr = retErr
		}
	}()
	_, info, data, err := s.readActiveFile()
	if s.active == nil {
		if os.IsNotExist(err) {
			return nil
		}
		return sceneError(s.lexical, errors.New("unexpected active marker"))
	}
	if err != nil {
		return sceneError(s.lexical, err)
	}
	if !os.SameFile(s.active.info, info) || s.active.data != data {
		return sceneError(s.lexical, errors.New("active marker changed"))
	}
	return nil
}

func writeProbeActive(s *overlayScene, launching bool, target *SessionProcess) (retErr error) {
	if s == nil {
		return nil
	} // Standalone round tests have no scene.
	s.activeKnown = true
	if err := s.checkActiveIdentity(); err != nil {
		return err
	}
	if s.absent {
		return sceneError(s.lexical, errors.New("active scene is missing"))
	}
	boot, err := readProbeBootID()
	if err != nil {
		return err
	}
	owner, err := identifySessionProcess(os.Getpid(), 0)
	if err != nil {
		return err
	}
	b, err := json.Marshal(probeActiveRecord{Version: 1, BootID: boot, Owner: owner, Target: target, Launching: launching})
	if err != nil {
		return err
	}
	name := ".active-" + randSuffix()
	if err := s.checkControlFile(name); err != nil {
		return err
	}
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	checkTemp := func() (retErr error) {
		defer func() {
			if retErr != nil {
				s.activeErr = retErr
			}
		}()
		if err := s.checkControlFile(name); err != nil {
			return err
		}
		got, err := s.root.Lstat(name)
		if err != nil || !os.SameFile(info, got) {
			return sceneError(s.lexical, errors.New("active temporary file changed"))
		}
		return nil
	}
	published := false
	defer func() {
		f.Close()
		if !published {
			if err := s.checkActiveIdentity(); err != nil {
				retErr = err
				return
			}
			if err := checkTemp(); err != nil {
				retErr = err
				return
			}
			if err := s.root.Remove(name); err != nil && !os.IsNotExist(err) {
				retErr = sceneError(s.lexical, err)
			}
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if sceneTestHook != nil {
		sceneTestHook("before-active-publish", filepath.Join(s.path, name))
	}
	if err := s.checkActiveIdentity(); err != nil {
		return err
	}
	if err := checkTemp(); err != nil {
		return err
	}
	if err := s.root.Rename(name, "active.json"); err != nil {
		return err
	}
	s.active = &probeActiveState{info: info, data: string(b)}
	published = true
	return forkSyncDir(s.root, ".")
}

func (s *overlayScene) clearActive() error {
	if err := s.checkActiveIdentity(); err != nil {
		return err
	}
	if s.absent || s.active == nil {
		return nil
	}
	if err := s.root.Remove("active.json"); err != nil {
		return sceneError(s.lexical, err)
	}
	s.active = nil
	return forkSyncDir(s.root, ".")
}

// Missing /proc identity proves exit; permission and parse failures do not.
func processIdentityGone(p SessionProcess) bool {
	if p.PID <= 0 || p.Start == 0 || p.UID != os.Getuid() {
		return false
	}
	now, err := procStartTime(p.PID)
	return errors.Is(err, os.ErrNotExist) || (err == nil && now != p.Start)
}

// A reaped leader alone is insufficient: strace may have left a bwrap child.
// Zombies cannot access the scene. All other group members must disappear.
func sessionGroupGone(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		return true
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, ent := range ents {
		if _, err := strconv.Atoi(ent.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false
		}
		i := strings.LastIndexByte(string(b), ')')
		if i < 0 {
			return false
		}
		fields := strings.Fields(string(b[i+1:]))
		if len(fields) < 3 {
			return false
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false
		}
		if group == pgid && fields[0] != "Z" && fields[0] != "X" {
			return false
		}
	}
	return true
}

func probeSceneActive(work string) bool {
	scene, err := openOverlayScene(work)
	if err != nil {
		return true
	}
	defer scene.close()
	return scene.probeActive()
}

func (s *overlayScene) probeActive() bool {
	if s.activeKnown {
		if err := s.checkActiveIdentity(); err != nil {
			return true
		}
		if sceneTestHook != nil {
			sceneTestHook("after-active-check", s.path)
		}
		if s.active == nil {
			return false
		}
		var rec probeActiveRecord
		if err := json.Unmarshal([]byte(s.active.data), &rec); err != nil {
			s.activeErr = sceneError(s.lexical, err)
			return true
		}
		boot, err := readProbeBootID()
		return err != nil || probeRecordActive(rec, boot)
	}
	rec, info, data, err := s.readActiveFile()
	s.activeKnown = true
	if os.IsNotExist(err) {
		s.active = nil
		return false
	}
	if err != nil {
		s.activeErr = sceneError(s.lexical, err)
		return true
	}
	s.active = &probeActiveState{info: info, data: data}
	boot, err := readProbeBootID()
	if err != nil {
		return true
	}
	return probeRecordActive(rec, boot)
}

func probeRecordActive(r probeActiveRecord, boot string) bool {
	if r.Version != 1 || r.Owner.PID <= 0 || r.Owner.Start == 0 || r.Owner.UID != os.Getuid() || r.Owner.PGID != 0 {
		return true
	}
	if r.Target != nil && (r.Launching || r.Target.PID <= 0 || r.Target.Start == 0 || r.Target.UID != os.Getuid() || r.Target.PGID != r.Target.PID) {
		return true
	}
	if !validProbeBootID(boot) {
		return true
	}
	if r.BootID != "" {
		if !validProbeBootID(r.BootID) {
			return true
		}
		if r.BootID != boot {
			return false
		}
	}
	if !processIdentityGone(r.Owner) || r.Launching {
		return true
	}
	return r.Target != nil && (!processIdentityGone(*r.Target) || !sessionGroupGone(r.Target.PGID))
}

// The marker and scene outlive the caller and the CLI process. A background
// waiter releases the in-process gate only after Wait and group exit are proven.
func retainUnreapedProbe(scene *overlayScene, res SessionResult, cleanup func() error) {
	if scene == nil {
		return
	}
	work := scene.lexical
	unreapedProbes.Store(work, true)
	if res.Reaped == nil || res.Process.PID <= 0 {
		scene.close()
		return
	}
	go func() {
		defer scene.close()
		<-res.Reaped
		for !processIdentityGone(res.Process) || !sessionGroupGone(res.Process.PGID) {
			time.Sleep(time.Second)
		}
		if cleanup != nil && cleanup() != nil {
			return
		}
		if err := scene.clearActive(); err != nil {
			return
		}
		unreapedProbes.Delete(work)
	}()
}

func validProbeBootID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func readProbeBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if !validProbeBootID(id) {
		return "", errors.New("invalid kernel boot identity")
	}
	return id, nil
}
