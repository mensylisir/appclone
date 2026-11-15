package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type auditEvidence struct {
	scene, source *overlayScene
	upper         string
	work          bool             // Compatibility audit roots also hold permanent evidence.
	peers         []*auditEvidence // Selected evidence owns all unselected candidates.
}

type auditCandidate struct {
	path string
	work bool
}

var auditEvidenceTestHook func(string) // nil outside synthetic fault tests

func (e *Env) auditCandidates(app, id string) []auditCandidate {
	paths := []auditCandidate{{e.AuditWorkDir(app, id), true}}
	if e.TmpRoot == "" {
		paths = append(paths, auditCandidate{filepath.Join(legacyTmpRoot, "appclone", fmt.Sprintf("audit.%d", e.UID), app+"."+id), true})
	}
	return append(paths, auditCandidate{e.AuditDir(app, id), false})
}

func (ev *auditEvidence) close() {
	for _, peer := range ev.peers {
		peer.close()
	}
	ev.peers = nil
	ev.scene.close()
}

// Freeze every location before reading or moving any evidence.
func (e *Env) openAuditScenes(app, id string) ([]*auditEvidence, error) {
	var out []*auditEvidence
	fail := func(err error) ([]*auditEvidence, error) {
		for _, ev := range out {
			ev.close()
		}
		return nil, err
	}
	for _, candidate := range e.auditCandidates(app, id) {
		scene, err := openOverlayScene(candidate.path)
		if err != nil {
			return fail(err)
		}
		duplicate := false
		for _, ev := range out {
			if ev.scene.path == scene.path || (ev.scene.info != nil && scene.info != nil && os.SameFile(ev.scene.info, scene.info)) {
				// A root also holding permanent evidence is never disposable.
				ev.work = ev.work && candidate.work
				duplicate = true
				break
			}
		}
		if duplicate {
			scene.close()
			continue
		}
		out = append(out, &auditEvidence{scene: scene, upper: filepath.Join(scene.lexical, "upper"), work: candidate.work})
	}
	return out, nil
}

func (ev *auditEvidence) captureUpper() error {
	if err := ev.scene.checkUnmounted(); err != nil {
		return err
	}
	if err := ev.scene.captureUpper(); err != nil {
		return err
	}
	ev.source = ev.scene.upper
	return ev.check()
}

func (ev *auditEvidence) check() error {
	if err := ev.checkOne(); err != nil {
		return err
	}
	for _, peer := range ev.peers {
		if err := peer.checkOne(); err != nil {
			return err
		}
	}
	return nil
}

func (ev *auditEvidence) checkOne() error {
	if err := ev.scene.checkUnmounted(); err != nil {
		return err
	}
	if ev.source == nil || !IsUnder(ev.scene.path, ev.source.path) {
		return sceneError(ev.upper, errors.New("evidence source was not captured"))
	}
	if ev.scene.absent && ev.source.absent {
		return nil
	}
	return ev.source.checkUnmounted()
}

func (e *Env) openAuditEvidence(app, id string) (*auditEvidence, error) {
	all, err := e.openAuditScenes(app, id)
	if err != nil {
		return nil, err
	}
	var selected *auditEvidence
	retained := false
	defer func() {
		if !retained {
			for _, ev := range all {
				ev.close()
			}
		}
	}()
	for _, ev := range all {
		if err := ev.captureUpper(); err != nil {
			selected = nil
			return nil, err
		}
		if !ev.source.absent {
			if selected != nil {
				selected = nil
				return nil, sceneError(ev.upper, errors.New("multiple unconsumed audit sources"))
			}
			selected = ev
		}
	}
	if selected == nil {
		selected = all[len(all)-1]
	}
	for _, ev := range all {
		if ev != selected {
			selected.peers = append(selected.peers, ev)
		}
	}
	retained = true
	return selected, nil
}

func (ev *auditEvidence) read() ([]UpperEntry, bool, error) {
	if err := ev.check(); err != nil {
		return nil, false, err
	}
	entries, present, err := ev.scene.readUpper()
	if checkErr := ev.check(); checkErr != nil {
		return nil, false, checkErr
	}
	return entries, present, err
}

func (ev *auditEvidence) removeWork() error {
	if err := ev.check(); err != nil {
		return err
	}
	if !ev.work {
		return nil
	}
	return removeAuditScene(ev.scene)
}
