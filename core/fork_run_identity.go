package core

import (
	"errors"
	"os"
	"path/filepath"
)

// Freeze aliases before reading instance metadata or waiting for any lock.
// The same identity accompanies all preparation and cleanup for this run.
type forkRunIdentity struct {
	app, id        string
	base, instance forkRunPath
	parents        []forkRunPath
}

type forkRunPath struct {
	lexical, canonical string
	info               os.FileInfo
}

func captureForkRunPath(path string) forkRunPath {
	real, err := checkedOrdinaryPath(path, true)
	if err != nil {
		return forkRunPath{lexical: path}
	}
	info, _ := os.Lstat(real)
	return forkRunPath{lexical: path, canonical: real, info: info}
}

func (e *Env) captureForkRun(app, id string) *forkRunIdentity {
	return &forkRunIdentity{app: app, id: id,
		base:     captureForkRunPath(e.Base),
		instance: captureForkRunPath(e.InstancePath(app, id)),
		parents: []forkRunPath{
			captureForkRunPath(filepath.Join(e.Base, "instances")),
			captureForkRunPath(filepath.Join(e.Base, "instances", app)),
		},
	}
}

func (identity *forkRunIdentity) check() error {
	if identity == nil {
		return nil
	}
	if err := ValidateAppKey(identity.app); err != nil {
		return err
	}
	if err := ValidateID(identity.id); err != nil {
		return err
	}
	rel := filepath.Join("instances", identity.app, identity.id)
	if identity.instance.canonical != filepath.Join(identity.base.canonical, rel) {
		return errors.New("instance storage escapes its declared layout")
	}
	paths := append([]forkRunPath{identity.base, identity.instance}, identity.parents...)
	table, err := checkedMountTable()
	if err != nil {
		return err
	}
	for _, path := range paths {
		canonical, err := ordinaryPath(table, path.lexical, true)
		if err != nil || canonical != path.canonical {
			return errors.New("instance storage alias changed")
		}
		if err := checkOrdinaryMounts(table, path.canonical); err != nil {
			return err
		}
		got, err := os.Lstat(path.canonical)
		if err != nil {
			return err
		}
		if path.info == nil || !got.IsDir() || !os.SameFile(path.info, got) {
			return errors.New("instance storage identity changed")
		}
	}
	root, err := openForkRoot(identity.base.canonical)
	if err != nil {
		return err
	}
	defer root.Close()
	baseInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(identity.base.info, baseInfo) {
		return errors.New("instance base changed while opening")
	}
	if err := checkedRootPath(root, identity.base.canonical); err != nil {
		return err
	}
	// Each component must be an ordinary directory, even if a symlink stays
	// inside Base. Check opened child identities while descending.
	parent := root
	for _, part := range []string{"instances", identity.app, identity.id} {
		child, err := openForkChild(parent, part)
		if err != nil {
			return err
		}
		defer child.Close()
		parent = child
	}
	got, err := parent.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(identity.instance.info, got) {
		return errors.New("instance storage changed while opening")
	}
	return checkedRootPath(root, identity.base.canonical)
}

// Explicit configuration copy: mutexes and atomic process state are not copied.
// flock still coordinates this private run context with other Env instances.
func (e *Env) withForkRun(identity *forkRunIdentity) *Env {
	return &Env{
		Base: identity.base.canonical, Resolver: e.Resolver, UID: e.UID,
		Clock: e.Clock, Trasher: e.Trasher, CliPath: e.CliPath,
		AppDirList: e.AppDirList, Mounter: e.Mounter, RunCmd: e.RunCmd,
		TmpRoot: e.TmpRoot, ProbeCapsOverride: e.ProbeCapsOverride,
		KeyringCapsOverride: e.KeyringCapsOverride,
		forkTestHook:        e.forkTestHook, forkRun: identity,
	}
}

// Internal entry points used without Run must capture once, then pass it on.
func (e *Env) pinForkInstance(in Instance) (*Env, Instance, error) {
	identity := e.forkRun
	if identity == nil {
		identity = e.captureForkRun(in.App, in.ID)
	}
	if in.App != identity.app || in.ID != identity.id ||
		(in.Root != filepath.Join(identity.instance.lexical, "root") &&
			in.Root != filepath.Join(identity.instance.canonical, "root")) {
		return nil, in, errors.New("instance does not belong to this run")
	}
	if err := identity.check(); err != nil {
		return nil, in, err
	}
	in.Root = filepath.Join(identity.instance.canonical, "root")
	if e.forkRun != nil {
		return e, in, nil
	}
	return e.withForkRun(identity), in, nil
}
