package core

import (
	"context"
	"os"
	"time"
)

// Namespace writers already hold OpenState (CLI/GUI transaction boundary).
// Re-read on every save/create so a profile prepared before migration cannot
// silently lower the schema and let an older binary use live fork lowers.
func (e *Env) preserveProfileVersion(p *Profile) error {
	if err := ValidateAppKey(p.App); err != nil {
		return err
	}
	p.Version = max(p.Version, ProfileVersion)
	data, err := os.ReadFile(e.ProfilePath(p.App))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		old, err := parseProfile(p.App, e.ProfilePath(p.App), data)
		if err != nil {
			return err
		}
		p.Version = max(p.Version, old.Version)
	}
	if p.Version > MaxProfileVersion {
		return errInput("err.profile.version_ahead", map[string]any{"app": p.App, "version": p.Version, "current": MaxProfileVersion})
	}
	return nil
}

// Called with run.lock held. Delete takes its run lock nonblocking, so it cannot
// wait on this run while holding OpenState. No global lock spans snapshot I/O.
func (e *Env) promoteForkProfile(ctx context.Context, app string) (Profile, error) {
	e.forkCheckpoint("before-profile-lock")
	s, err := e.OpenState(ctx, 5*time.Second)
	if err != nil {
		return Profile{}, err
	}
	defer s.Close()
	if err := e.forkRun.check(); err != nil {
		return Profile{}, forkPreparationError(err)
	}
	p, err := e.LoadProfile(app)
	if err != nil {
		return p, err
	}
	if p.Version < 2 {
		p.Version = 2
		if err := e.forkRun.check(); err != nil {
			return p, forkPreparationError(err)
		}
		if err := e.SaveProfile(p); err != nil {
			return p, err
		}
	}
	// A durable schema barrier precedes any published fixed snapshot.
	d, err := os.Open(e.Base + "/profiles.d")
	if err != nil {
		return p, err
	}
	defer d.Close()
	return p, d.Sync()
}
