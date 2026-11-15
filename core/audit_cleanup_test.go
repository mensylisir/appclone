package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestAuditMountAndStopFailurePreserveScene(t *testing.T) {
	for _, failure := range []string{"mount", "abandon", "finish"} {
		t.Run(failure, func(t *testing.T) {
			e := probeEnv(t)
			app, id := seedVerifyApp(t, e)
			in, err := e.GetInstance(app, id)
			if err != nil {
				t.Fatal(err)
			}
			p, err := e.LoadProfile(app)
			if err != nil {
				t.Fatal(err)
			}
			m := &failedSceneMounter{t: t, table: forkTableFixture(t), failMount: failure == "mount", failUnmount: failure != "mount"}
			e.Mounter = m
			sess, _, err := e.beginAudit(in, p, nil)
			if failure == "mount" {
				if err == nil || sess != nil {
					t.Fatal("failed mount accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if failure == "finish" {
					err = sess.finish(0)
				} else {
					err = sess.abandon(false)
				}
				if err == nil || sess.detach() == nil {
					t.Fatal("unmount failure lost")
				}
			}
			assertTestFile(t, filepath.Join(m.scene, "merged", "sentinel"), "preserve")
			rec, present, err := readRunRecord(filepath.Join(e.AuditDir(app, id), "run.toml"))
			if err != nil || !present || rec.complete() {
				t.Fatal("failed audit was certified complete")
			}
		})
	}
}

func TestVerifyRefusesReplacedUpperAfterJudging(t *testing.T) {
	for _, source := range []string{"current", "compat"} {
		for _, leak := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/leak=%v", source, leak), func(t *testing.T) {
				e := probeEnv(t)
				app, id := seedVerifyApp(t, e)
				work := e.AuditWorkDir(app, id)
				if source == "compat" {
					work = e.AuditDir(app, id)
				}
				upper := filepath.Join(work, "upper")
				rel := ".cache/TestApp/blob"
				if leak {
					rel = ".config/Unrecognized/state"
				}
				writeTestFile(t, filepath.Join(upper, rel), "original")
				seedRunRecord(t, filepath.Join(e.AuditDir(app, id), "run.toml"), runRecord{App: app, ID: id})
				old := auditEvidenceTestHook
				fired := false
				auditEvidenceTestHook = func(stage string) {
					if stage != "after-judge" {
						return
					}
					fired = true
					if err := os.Rename(upper, upper+".old"); err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, filepath.Join(upper, "sentinel"), "replacement")
				}
				t.Cleanup(func() { auditEvidenceTestHook = old })
				if _, err := e.Verify(app, id); err == nil || !fired {
					t.Fatal("replacement upper consumed, or judgment not reached")
				}
				assertTestFile(t, filepath.Join(upper+".old", rel), "original")
				assertTestFile(t, filepath.Join(upper, "sentinel"), "replacement")
				if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "summary.toml")); !os.IsNotExist(err) {
					t.Fatal("wrote success summary after upper replacement")
				}
			})
		}
	}
}

// Exercise the production legacy selector while owning a unique, empty audit
// namespace. Application HOME/Base/XDG remain synthetic; no real UID's audit
// namespace is enumerated or modified.
func legacyAuditFixture(t *testing.T, e *Env) {
	t.Helper()
	parent := filepath.Join(DefaultTmpRoot(), "appclone")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(parent, "audit.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	uid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(root), "audit."))
	if err != nil || "audit."+strconv.Itoa(uid) != filepath.Base(root) {
		t.Fatal("temporary audit namespace is not numeric")
	}
	e.UID = uid
	e.TmpRoot = ""
	old := legacyTmpRoot
	legacyTmpRoot = t.TempDir()
	t.Cleanup(func() { legacyTmpRoot = old })
}

func TestAuditRetiresEveryOldSourceBeforeNewRecord(t *testing.T) {
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	legacyAuditFixture(t, e)
	forkTableFixture(t)
	in, err := e.GetInstance(app, id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	paths := e.auditCandidates(app, id)
	for i, candidate := range paths {
		writeTestFile(t, filepath.Join(candidate.path, "upper", fmt.Sprintf("source-%d", i)), "preserve")
	}
	permanent := filepath.Join(e.AuditDir(app, id), "quarantine-keep", "sentinel")
	writeTestFile(t, permanent, "permanent")
	seedRunRecord(t, filepath.Join(e.AuditDir(app, id), "run.toml"), runRecord{App: app, ID: id})
	if _, err := e.Verify(app, id); err == nil {
		t.Fatal("multiple old sources associated with one record")
	}
	e.Mounter = &fakeMounter{}
	sess, _, err := e.beginAudit(in, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sess.abandon(false); err != nil {
			t.Error(err)
		}
	}()
	for i, candidate := range paths {
		if _, err := os.Stat(filepath.Join(candidate.path, "upper", fmt.Sprintf("source-%d", i))); !os.IsNotExist(err) {
			t.Fatal("old source mixed into new audit")
		}
	}
	qs, err := filepath.Glob(filepath.Join(e.AuditDir(app, id), "quarantine-*"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range paths {
		found := 0
		for _, q := range qs {
			if _, err := os.Stat(filepath.Join(q, fmt.Sprintf("source-%d", i))); err == nil {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("source %d has %d complete preserved copies", i, found)
		}
	}
	assertTestFile(t, permanent, "permanent")
	rec, present, err := readRunRecord(filepath.Join(e.AuditDir(app, id), "run.toml"))
	if err != nil || !present || rec.complete() {
		t.Fatal("new in-progress record not written")
	}
}

func TestAuditIncompleteSourceMoveKeepsOldRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary permission checks")
	}
	e := probeEnv(t)
	app, id := seedVerifyApp(t, e)
	upper := filepath.Join(e.AuditDir(app, id), "upper")
	locked := filepath.Join(upper, "locked")
	writeTestFile(t, filepath.Join(locked, "sentinel"), "preserve")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	record := filepath.Join(e.AuditDir(app, id), "run.toml")
	seedRunRecord(t, record, runRecord{App: app, ID: id})
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	oldRename, oldChmod := moveRename, removeChmod
	moveRename = func(string, string) error { return syscall.EXDEV }
	removeChmod = func(string, os.FileMode) error { return syscall.EPERM }
	t.Cleanup(func() { moveRename, removeChmod = oldRename, oldChmod })
	in, err := e.GetInstance(app, id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadProfile(app)
	if err != nil {
		t.Fatal(err)
	}
	e.Mounter = &fakeMounter{}
	if _, _, err := e.beginAudit(in, p, nil); err == nil {
		t.Fatal("complete quarantine authorized reuse of incomplete old source")
	}
	assertTestFile(t, record, string(before))
	qs, err := filepath.Glob(filepath.Join(e.AuditDir(app, id), "quarantine-*"))
	if err != nil || len(qs) != 1 {
		t.Fatal("complete backup not retained")
	}
	assertTestFile(t, filepath.Join(qs[0], "locked", "sentinel"), "preserve")
}

func TestVerifyNeverConsumesMountedEvidence(t *testing.T) {
	for _, source := range []string{"current", "compat"} {
		t.Run(source, func(t *testing.T) {
			e := probeEnv(t)
			app, id := seedVerifyApp(t, e)
			scene := e.AuditWorkDir(app, id)
			if source == "compat" {
				scene = e.AuditDir(app, id)
			}
			writeTestFile(t, filepath.Join(scene, "upper", ".cache", "TestApp", "blob"), "preserve")
			seedRunRecord(t, filepath.Join(e.AuditDir(app, id), "run.toml"), runRecord{App: app, ID: id})
			m := forkTableFixture(t)
			m.points = []string{filepath.Join(scene, "merged")}
			m.save()
			if _, err := e.Verify(app, id); err == nil {
				t.Fatal("mounted evidence accepted")
			}
			assertTestFile(t, filepath.Join(scene, "upper", ".cache", "TestApp", "blob"), "preserve")
			if _, err := os.Stat(filepath.Join(e.AuditDir(app, id), "summary.toml")); !os.IsNotExist(err) {
				t.Fatal("unsafe evidence wrote a consumption summary")
			}
		})
	}
}
