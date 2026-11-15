package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func sharedPathsEnv(t *testing.T) *Env {
	t.Helper()
	e := testEnv(t)
	runtime := e.Resolver.XDG["RUNTIME_DIR"]
	e.Resolver = newResolver(e.Resolver.Home, e.Resolver.Home, 1000, func(key string) string {
		if key == "XDG_RUNTIME_DIR" {
			return runtime
		}
		return ""
	})
	return e
}

func TestPreciseSharedClassification(t *testing.T) {
	r := classifyEnv(t)
	for _, rel := range []string{
		".config/mimeapps.list", ".config/gnome-mimeapps.list", ".config/kde-mimeapps.list",
		".local/share/applications", ".local/share/applications/demo.desktop",
		".local/share/applications/mimeinfo.cache", ".pki/nssdb/key4.db", ".local/share/pki/key4.db",
	} {
		if cls, checked, _, _ := r.Classify(filepath.Join(r.Home, rel)); cls != ClassShared || checked {
			t.Errorf("%s: got %s checked=%v", rel, cls, checked)
		}
	}
	for _, rel := range []string{
		".config/App/mimeapps.list", ".config/App/gnome-mimeapps.list", ".config/App/applications",
		".config/foo-mimeapps.list.backup", ".config/-mimeapps.list", ".local/share/applications-private",
		".config/App/pki/key4.db",
	} {
		if cls, checked, _, _ := r.Classify(filepath.Join(r.Home, rel)); cls != ClassConfig || !checked {
			t.Errorf("private %s: got %s checked=%v", rel, cls, checked)
		}
	}
}

func TestSharedDesktopEvidenceRoutesAndZeroGate(t *testing.T) {
	e := sharedPathsEnv(t)
	r := e.Resolver
	rels := []string{".config/mimeapps.list", ".config/gnome-mimeapps.list", ".local/share/applications/demo.desktop", ".local/share/pki/nssdb/key4.db"}
	var entries []UpperEntry
	for _, rel := range rels {
		entries = append(entries, UpperEntry{Path: rel, Kind: EntryFile})
	}
	for _, source := range []string{"write", "read", "both"} {
		t.Run(source, func(t *testing.T) {
			writes, reads := entries, rels
			if source == "write" {
				reads = nil
			}
			if source == "read" {
				writes = nil
			}
			cands, _, _, _ := r.MergeUpperWithReads(writes, reads, nil, e.Base, "demo")
			p := r.DeriveProfile(Profile{App: "demo"}, nil, "overlay", ReadTraceOK, cands)
			if len(p.Paths) != 0 || len(p.CredentialPaths) != 0 || len(p.SharedWrites) != 4 || !r.HasSharedCertificates(p) {
				t.Fatalf("shared evidence misrouted: %+v", p)
			}
			for _, c := range cands {
				if c.Class != ClassShared || c.Checked || c.Credential != CredentialNone || c.ContainsCredential {
					t.Fatalf("shared credential upgraded: %+v", c)
				}
			}
			res := ProbeResult{Profile: p, Candidates: cands, ReadTrace: ReadTraceOK}
			if _, _, _, err := e.FinalizeProbe("demo", res, ProbeFinalize{}); errKey(err) != "err.probe.no_writes" {
				t.Fatalf("shared-only profile accepted: %v", err)
			}
		})
	}
}

func TestPreciseSharedAliasNeverPromotesPrivateOwner(t *testing.T) {
	for _, kind := range []string{"pki", "applications", "nested-xdg"} {
		for _, source := range []string{"write", "read", "prefill"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				e := sharedPathsEnv(t)
				r := e.Resolver
				appDir := filepath.Join(r.XDG["CONFIG_HOME"], "应用 App")
				shared := filepath.Join(appDir, "shared")
				writeTestFile(t, filepath.Join(shared, "key4.db"), "synthetic")
				writeTestFile(t, filepath.Join(appDir, "token"), "synthetic private")
				alias := filepath.Join(r.Home, ".pki")
				if kind == "applications" {
					alias = filepath.Join(r.XDG["DATA_HOME"], "applications")
				}
				if kind == "nested-xdg" {
					r.XDG["DATA_HOME"] = appDir
					alias = filepath.Join(appDir, "pki")
				}
				if err := os.MkdirAll(filepath.Dir(alias), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(shared, alias); err != nil {
					t.Fatal(err)
				}
				rel := func(abs string) string {
					v, err := filepath.Rel(r.Home, abs)
					if err != nil {
						t.Fatal(err)
					}
					return v
				}
				var cands []Candidate
				switch source {
				case "write":
					cands, _ = r.MergeUpper([]UpperEntry{{Path: rel(appDir), Kind: EntryDir}, {Path: rel(shared), Kind: EntryDir}, {Path: rel(filepath.Join(shared, "key4.db")), Kind: EntryFile}})
				case "read":
					cands, _, _, _ = r.MergeUpperWithReads(nil, []string{rel(filepath.Join(shared, "key4.db"))}, nil, e.Base, "demo")
				case "prefill":
					cands = r.PrefillCandidates(Profile{Paths: []string{alias, filepath.Join(shared, "key4.db")}}, nil)
				}
				if len(cands) != 1 || cands[0].Path != shared || cands[0].Class != ClassShared || cands[0].Checked {
					t.Fatalf("shared scope expanded/duplicated: %+v", cands)
				}
				// A genuine private sibling still produces its owner. Do not share it
				// or silently drop it to resolve the alias conflict.
				switch source {
				case "write":
					cands, _ = r.MergeUpper([]UpperEntry{{Path: rel(appDir), Kind: EntryDir}, {Path: rel(shared), Kind: EntryDir}, {Path: rel(filepath.Join(shared, "key4.db")), Kind: EntryFile}, {Path: rel(filepath.Join(appDir, "token")), Kind: EntryFile}})
				case "read":
					cands, _, _, _ = r.MergeUpperWithReads([]UpperEntry{{Path: rel(filepath.Join(appDir, "token")), Kind: EntryFile}}, []string{rel(filepath.Join(shared, "key4.db"))}, nil, e.Base, "demo")
				case "prefill":
					private, _ := r.MergeUpper([]UpperEntry{{Path: rel(filepath.Join(appDir, "token")), Kind: EntryFile}})
					cands = append(cands, private...)
				}
				p := r.DeriveProfile(Profile{App: "demo"}, nil, "overlay", ReadTraceOK, cands)
				if len(p.Paths) != 1 || !slices.Contains(p.SharedWrites, r.ToStoreForm(shared)) || slices.Contains(p.SharedWrites, r.ToStoreForm(appDir)) {
					t.Fatalf("private sibling lost or shared: %+v", p)
				}
				_, _, _, err := e.FinalizeProbe("demo", ProbeResult{Profile: p, Candidates: cands, ReadTrace: ReadTraceOK}, ProbeFinalize{})
				if errKey(err) != "err.profile.isolate_and_share" {
					t.Fatalf("missing conflict refusal: %v", err)
				}
				if _, err := os.Stat(filepath.Join(e.Base, "profiles.d", "demo.toml")); !os.IsNotExist(err) {
					t.Fatalf("unexpected profile state: %v", err)
				}
				if insts, err := e.ListInstances("demo"); err != nil || len(insts) != 0 {
					t.Fatalf("instance created: %v %v", insts, err)
				}
				assertTestFile(t, filepath.Join(appDir, "token"), "synthetic private")
			})
		}
	}
}

func TestSharedPathsWithCustomXDGAndHomeAliases(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "中文 home")
	if err := os.MkdirAll(filepath.Join(realHome, "配置"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realHome, "数据"), 0700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "alias home")
	if err := os.Symlink(realHome, home); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "cfg-link")
	if err := os.Symlink(filepath.Join(realHome, "配置"), config); err != nil {
		t.Fatal(err)
	}
	r := newResolver(home, home, 1000, func(key string) string {
		switch key {
		case "XDG_CONFIG_HOME":
			return config
		case "XDG_DATA_HOME":
			return filepath.Join(home, "数据")
		}
		return ""
	})
	for _, raw := range []string{filepath.Join(config, "mimeapps.list"), filepath.Join(config, "gnome-mimeapps.list"), filepath.Join(home, "数据", "applications"), filepath.Join(home, "数据", "pki")} {
		for _, path := range []string{raw, Real(raw)} {
			if cls, checked, _, _ := r.Classify(path); cls != ClassShared || checked {
				t.Fatalf("alias %s: %s/%v", path, cls, checked)
			}
		}
	}
}

func TestSharedReprobeKeepsOldExplicitProfileAndVersion(t *testing.T) {
	e := sharedPathsEnv(t)
	app, old := seedApp(t, e)
	old.Version = 2
	old.Paths = append(old.Paths, "~/.config/mimeapps.list", "~/.local/share/pki")
	old.Audit = true
	writeTestFile(t, filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "mimeapps.list"), "synthetic associations")
	if err := e.SaveProfile(old); err != nil {
		t.Fatal(err)
	}
	before, err := e.CreateInstanceWith(context.Background(), app, old, "existing", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(before.Root, "sentinel"), "private retained")
	loaded, err := e.LoadProfile(app)
	if err != nil || !slices.Equal(loaded.Paths, old.Paths) {
		t.Fatalf("old paths changed: %v %v", loaded.Paths, err)
	}
	r := e.Resolver
	if r.HasSharedCertificates(loaded) {
		t.Fatal("explicit isolated certificate path described as shared")
	}
	cands, _ := r.MergeUpper([]UpperEntry{{Path: ".config/Demo/state", Kind: EntryFile}, {Path: ".config/mimeapps.list", Kind: EntryFile}, {Path: ".local/share/pki/key4.db", Kind: EntryFile}})
	probe := r.DeriveProfile(Profile{App: app, Exec: old.Exec}, nil, "overlay", ReadTraceOK, cands)
	p, _, _, err := e.FinalizeProbe(app, ProbeResult{Profile: probe, Candidates: cands, ReadTrace: ReadTraceOK}, ProbeFinalize{Existing: old, HasExisting: true})
	if err != nil || p.Version != 2 || !p.Audit || len(p.Paths) != 1 || !r.HasSharedCertificates(p) {
		t.Fatalf("reprobe wrong: %+v %v", p, err)
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, filepath.Join(before.Root, "sentinel"), "private retained")
}

func TestForkSharedCertificatesBlockNewAndExistingSnapshot(t *testing.T) {
	for _, store := range []string{"home", "data"} {
		for _, existing := range []bool{false, true} {
			t.Run(store+"/"+map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
				e, p, in, abs := snapshotFixture(t)
				writeTestFile(t, filepath.Join(abs, "nssdb", "key4.db"), "synthetic")
				upper, _ := instanceChild(in.Root, abs)
				writeTestFile(t, filepath.Join(upper, "private"), "clone retained")
				if existing && snapshotForTest(t, e, p, in)[abs] == "" {
					t.Fatal("missing original snapshot")
				}
				alias := filepath.Join(e.Resolver.Home, ".pki")
				if store == "data" {
					alias = filepath.Join(e.Resolver.XDG["DATA_HOME"], "pki")
				}
				if err := os.MkdirAll(filepath.Dir(alias), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(abs, "nssdb"), alias); err != nil {
					t.Fatal(err)
				}
				if got := snapshotForTest(t, e, p, in)[abs]; got != "" {
					t.Fatalf("inherited shared certificate owner: %s", got)
				}
				assertTestFile(t, filepath.Join(upper, "private"), "clone retained")
				assertTestFile(t, filepath.Join(abs, "nssdb", "key4.db"), "synthetic")
			})
		}
	}
}

// Real probe and run: shared desktop state stays in the probe overlay, then
// shares the synthetic host paths at run time. No real application data is used.
func TestSharedDesktopProbeRunE2E(t *testing.T) {
	if os.Getenv("APPCLONE_SHARED_E2E") != "1" {
		t.Skip("set APPCLONE_SHARED_E2E=1 for real shared-path acceptance")
	}
	e := sharedPathsEnv(t)
	e.UID, e.Resolver.UID = os.Getuid(), os.Getuid()
	t.Setenv("HOME", e.Resolver.Home)
	t.Setenv("AC_HOME", e.Resolver.Home)
	t.Setenv("AC_BASE", e.Base)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	for key, value := range e.Resolver.XDG {
		t.Setenv("XDG_"+key, value)
		if err := os.MkdirAll(value, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(e.Base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.Resolver.Home, "Downloads"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "user-dirs.dirs"), "XDG_DOWNLOAD_DIR=\"$HOME/Downloads\"\n")
	private := filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "演示 App")
	writeTestFile(t, filepath.Join(private, "host-only"), "private original")
	app := filepath.Join(t.TempDir(), "shared-demo")
	body := `#!/bin/sh
set -eu
mkdir -p "$XDG_CONFIG_HOME/演示 App" "$XDG_DATA_HOME/applications" "$HOME/.pki/nssdb" "$XDG_DATA_HOME/pki"
printf fixture > "$XDG_CONFIG_HOME/演示 App/written"
printf fixture > "$XDG_CONFIG_HOME/mimeapps.list"
printf fixture > "$XDG_DATA_HOME/applications/demo.desktop"
printf fixture > "$HOME/.pki/nssdb/key4.db"
printf fixture > "$XDG_DATA_HOME/pki/key4.db"
printf fixture > "$HOME/Downloads/shared"
`
	if err := os.WriteFile(app, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	appsDir := filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications")
	e.AppDirList = []string{appsDir}
	writeTestFile(t, filepath.Join(appsDir, "shared-demo.desktop"), "[Desktop Entry]\nType=Application\nName=Shared Demo\nExec="+app+"\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	capture := &probeCaptureRunner{}
	res, err := e.Probe(ctx, "shared-demo", ProbeOptions{Command: []string{app}, Runner: capture})
	if err != nil {
		t.Fatalf("probe: %v cause=%v", err, errors.Unwrap(err))
	}
	p, _, _, err := e.FinalizeProbe("shared-demo", res, ProbeFinalize{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Paths) != 1 || p.Paths[0] != e.Resolver.ToStoreForm(private) || len(p.SharedWrites) != 4 || len(p.CredentialPaths) != 0 {
		t.Fatalf("incorrect probe routes: %+v", p)
	}
	for _, raw := range []string{filepath.Join(private, "written"), filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "mimeapps.list"), filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications", "demo.desktop"), filepath.Join(e.Resolver.Home, ".pki", "nssdb", "key4.db"), filepath.Join(e.Resolver.XDG["DATA_HOME"], "pki", "key4.db")} {
		if _, err := os.Stat(raw); !os.IsNotExist(err) {
			t.Fatalf("probe wrote host fixture: %s: %v", raw, err)
		}
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	in, err := e.CreateInstanceWith(ctx, p.App, p, "main", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	run, err := e.Run(ctx, p.App, in.ID, RunOptions{Stderr: &stderr})
	if err != nil || run.AppExitCode != 0 {
		t.Fatalf("run: %+v %v cause=%v %s", run, err, errors.Unwrap(err), stderr.String())
	}
	upper, _ := instanceChild(in.Root, private)
	assertTestFile(t, filepath.Join(upper, "written"), "fixture")
	assertTestFile(t, filepath.Join(private, "host-only"), "private original")
	if _, err := os.Stat(filepath.Join(private, "written")); !os.IsNotExist(err) {
		t.Fatalf("run wrote host private data: %v", err)
	}
	for _, rel := range []string{"Downloads/shared", ".config/mimeapps.list", ".local/share/applications/demo.desktop", ".pki/nssdb/key4.db", ".local/share/pki/key4.db"} {
		assertTestFile(t, filepath.Join(e.Resolver.Home, rel), "fixture")
	}
	for _, p := range capture.processes {
		if !sessionGroupGone(p.PGID) {
			t.Fatalf("probe group remains: %d", p.PGID)
		}
	}
	if mounted, err := checkedMountUnder(e.TmpRoot); err != nil || mounted {
		t.Fatalf("probe mounts remain: %v %v", mounted, err)
	}
	if mounted, err := checkedMountUnder(in.Dir()); err != nil || mounted {
		t.Fatalf("run mounts remain: %v %v", mounted, err)
	}
}
