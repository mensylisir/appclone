package core

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSharedMIMEFileAliasRoutes(t *testing.T) {
	for _, route := range []string{"write", "read", "prefill"} {
		for _, canonical := range []bool{false, true} {
			t.Run(route+map[bool]string{false: "/lexical", true: "/canonical"}[canonical], func(t *testing.T) {
				e := sharedPathsEnv(t)
				r := e.Resolver
				app := filepath.Join(r.XDG["CONFIG_HOME"], "应用 App")
				target := filepath.Join(app, "associations.list")
				writeTestFile(t, target, "synthetic")
				alias := filepath.Join(r.XDG["CONFIG_HOME"], "gnome-mimeapps.list")
				if err := os.Symlink(target, alias); err != nil {
					t.Fatal(err)
				}
				path := alias
				if canonical {
					path = target
				}
				if cls, checked, _, _ := r.Classify(path); cls != ClassShared || checked {
					t.Fatalf("alias class=%s checked=%v", cls, checked)
				}
				rel := func(p string) string {
					s, err := filepath.Rel(r.Home, p)
					if err != nil {
						t.Fatal(err)
					}
					return s
				}
				var cands []Candidate
				switch route {
				case "write":
					cands, _ = r.MergeUpper([]UpperEntry{{Path: rel(app), Kind: EntryDir}, {Path: rel(path), Kind: EntryFile}})
				case "read":
					cands, _, _, _ = r.MergeUpperWithReads(nil, []string{rel(path)}, nil, e.Base, "demo")
				case "prefill":
					cands = r.PrefillCandidates(Profile{Paths: []string{path, alias, target}}, nil)
				}
				if len(cands) != 1 || cands[0].Path != target || cands[0].Class != ClassShared || cands[0].Checked {
					t.Fatalf("shared file misrouted: %+v", cands)
				}
				for _, kind := range []EntryKind{EntryFile, EntryDir} {
					private, _ := r.MergeUpper([]UpperEntry{{Path: rel(filepath.Join(app, "private")), Kind: kind}})
					mixed := append(append([]Candidate{}, cands...), private...)
					p := r.DeriveProfile(Profile{App: "demo"}, nil, "overlay", ReadTraceOK, mixed)
					if len(p.Paths) != 1 || p.Paths[0] != r.ToStoreForm(app) {
						t.Fatalf("private sibling lost: %+v", p)
					}
					if _, _, _, err := e.FinalizeProbe("demo", ProbeResult{Profile: p, Candidates: mixed, ReadTrace: ReadTraceOK}, ProbeFinalize{}); errKey(err) != "err.profile.isolate_and_share" {
						t.Fatalf("missing conflict: %v", err)
					}
				}
			})
		}
	}
}

func TestSharedMIMEInvalidMetadataCannotPublish(t *testing.T) {
	for _, kind := range []string{"directory", "link-directory", "fifo", "socket", "dangling", "unreadable-config", "file-config", "dangling-config"} {
		t.Run(kind, func(t *testing.T) {
			e := sharedPathsEnv(t)
			r := e.Resolver
			cfg := r.XDG["CONFIG_HOME"]
			if err := os.MkdirAll(cfg, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(cfg, "gnome-mimeapps.list")
			switch kind {
			case "directory":
				if err := os.Mkdir(file, 0700); err != nil {
					t.Fatal(err)
				}
			case "link-directory":
				target := filepath.Join(cfg, "App")
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, file); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(file, 0600); err != nil {
					t.Fatal(err)
				}
			case "socket":
				// Use a short temp path; long t.TempDir names exceed sockaddr_un.
				dir, err := os.MkdirTemp("", "ac-mime-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(dir) })
				listener, err := net.Listen("unix", filepath.Join(dir, "sock"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { listener.Close() })
				if err := os.Symlink(filepath.Join(dir, "sock"), file); err != nil {
					t.Fatal(err)
				}
			case "dangling":
				if err := os.Symlink(filepath.Join(cfg, "missing"), file); err != nil {
					t.Fatal(err)
				}
			case "unreadable-config":
				if os.Geteuid() == 0 {
					t.Skip("permission test requires ordinary uid")
				}
				if err := os.Chmod(cfg, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(cfg, 0700) })
			case "file-config", "dangling-config":
				if err := os.Remove(cfg); err != nil {
					t.Fatal(err)
				}
				if kind == "file-config" {
					writeTestFile(t, cfg, "synthetic")
				} else if err := os.Symlink(filepath.Join(r.Home, "missing"), cfg); err != nil {
					t.Fatal(err)
				}
			}
			cands := []Candidate{{Path: filepath.Join(cfg, "App"), Class: ClassConfig, Checked: true}}
			p, _, _, err := e.FinalizeProbe("demo", ProbeResult{Profile: Profile{App: "demo"}, Candidates: cands}, ProbeFinalize{})
			if err == nil || !reflect.DeepEqual(p, Profile{}) {
				t.Fatalf("invalid MIME state published: %+v %v", p, err)
			}
			if got := r.PrefillCandidates(Profile{Paths: []string{filepath.Join(cfg, "App")}}, nil); len(got) != 0 {
				t.Fatalf("invalid metadata emitted candidates: %+v", got)
			}
			if _, err := os.Stat(filepath.Join(e.Base, "profiles.d", "demo.toml")); !os.IsNotExist(err) {
				t.Fatalf("profile created: %v", err)
			}
		})
	}
}

func TestSharedMIMEBatchFailureStaysFailed(t *testing.T) {
	for _, kind := range []string{"read", "type"} {
		t.Run(kind, func(t *testing.T) {
			e := sharedPathsEnv(t)
			r := e.Resolver
			cfg := r.XDG["CONFIG_HOME"]
			target := filepath.Join(cfg, "App", "associations.list")
			writeTestFile(t, target, "synthetic")
			alias := filepath.Join(cfg, "gnome-mimeapps.list")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			var scoped *PathResolver
			if kind == "read" {
				if os.Geteuid() == 0 {
					t.Skip("permission test requires ordinary uid")
				}
				if err := os.Chmod(cfg, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(cfg, 0700) })
				scoped = r.withSharedMIME()
				if err := os.Chmod(cfg, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				scoped = r.withSharedMIME()
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if got := scoped.PrefillCandidates(Profile{Paths: []string{target}}, nil); len(got) != 0 {
					t.Fatal("changed type emitted candidates")
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, target, "synthetic")
			}
			first := scoped.sharedMIME.err
			if first == nil {
				t.Fatal("batch did not retain metadata failure")
			}
			if got, _ := scoped.MergeUpper([]UpperEntry{{Path: ".config/App/private", Kind: EntryFile}}); len(got) != 0 {
				t.Fatal("failed batch emitted private writes")
			}
			if got, _, _, _ := scoped.MergeUpperWithReads(nil, []string{".config/App/token"}, nil, e.Base, "demo"); len(got) != 0 {
				t.Fatal("failed batch emitted private reads")
			}
			if got := scoped.PrefillCandidates(Profile{Paths: []string{target}}, nil); len(got) != 0 || scoped.sharedMIME.err != first {
				t.Fatal("later recovery erased first error")
			}
			e.Resolver = scoped
			if _, _, _, err := e.FinalizeProbe("demo", ProbeResult{}, ProbeFinalize{}); err != first {
				t.Fatalf("finalize cleared error: %v", err)
			}
			if got := r.PrefillCandidates(Profile{Paths: []string{target}}, nil); len(got) != 1 || got[0].Class != ClassShared {
				t.Fatalf("fresh batch did not recover: %+v", got)
			}
			if r.sharedMIME != nil {
				t.Fatal("original resolver was mutated")
			}
		})
	}
}

func TestSharedMIMEFreshAliasAndMissingFiles(t *testing.T) {
	e := sharedPathsEnv(t)
	r := e.Resolver
	target := filepath.Join(r.XDG["CONFIG_HOME"], "App", "associations.list")
	if cls, _, _, _ := r.Classify(target); cls != ClassConfig {
		t.Fatal(cls)
	}
	for _, name := range []string{"mimeapps.list", "gnome-mimeapps.list"} {
		if cls, checked, _, _ := r.Classify(filepath.Join(r.XDG["CONFIG_HOME"], name)); cls != ClassShared || checked {
			t.Fatalf("future MIME file: %s %v", cls, checked)
		}
	}
	writeTestFile(t, target, "synthetic")
	if err := os.Symlink(target, filepath.Join(r.XDG["CONFIG_HOME"], "gnome-mimeapps.list")); err != nil {
		t.Fatal(err)
	}
	if cls, checked, _, _ := r.Classify(target); cls != ClassShared || checked {
		t.Fatalf("new alias missed: %s %v", cls, checked)
	}
}

func TestSharedMIMEProbeRejectsInvalidResult(t *testing.T) {
	e := probeEnv(t)
	if err := os.Mkdir(filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "gnome-mimeapps.list"), 0700); err != nil {
		t.Fatal(err)
	}
	m := &fakeMounter{entries: []string{".config/TestApp/state"}}
	runner := &fakeRunner{}
	res, err := e.Probe(context.Background(), "testapp", ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: runner})
	if errKey(err) != "err.probe.shared_mime" || !reflect.DeepEqual(res, ProbeResult{}) {
		t.Fatalf("invalid probe produced result: %+v %v", res, err)
	}
	if m.mounted {
		t.Fatal("probe left mount behind")
	}
	entries, readErr := os.ReadDir(e.TmpRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), "appclone-probe-") {
			t.Fatalf("probe work remains: %s", en.Name())
		}
	}
}

func TestSharedMIMEUpperTypes(t *testing.T) {
	for _, kind := range []string{"file", "directory", "fifo", "socket", "symlink", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			e := sharedPathsEnv(t)
			dir, err := os.MkdirTemp("", "ac-upper-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) }) // synthetic, never mounted
			writeTestFile(t, filepath.Join(dir, ".config", "App", "state"), "synthetic private")
			name := filepath.Join(dir, ".config", "gnome-mimeapps.list")
			want := EntryOther
			switch kind {
			case "file":
				writeTestFile(t, name, "synthetic")
				want = EntryFile
			case "directory":
				writeTestFile(t, filepath.Join(name, "child"), "synthetic")
				want = EntryDir
			case "fifo":
				if err := unix.Mkfifo(name, 0600); err != nil {
					t.Fatal(err)
				}
			case "socket":
				l, err := net.Listen("unix", name)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { l.Close() })
			case "symlink", "dangling":
				if kind == "symlink" {
					writeTestFile(t, filepath.Join(dir, "target"), "synthetic")
				}
				if err := os.Symlink("../target", name); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { root.Close() })
			entries, err := readUpperRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, en := range entries {
				if en.Path == ".config/gnome-mimeapps.list" {
					found = true
					if en.Kind != want {
						t.Fatalf("type lost: %+v", en)
					}
				}
			}
			if !found {
				t.Fatal("MIME evidence missing")
			}
			r := e.Resolver.withSharedMIME()
			cands, _, _, _ := r.MergeUpperWithReads(entries, nil, nil, e.Base, "demo")
			if kind != "file" {
				if errKey(r.sharedMIME.err) != "err.probe.shared_mime" || len(cands) != 0 {
					t.Fatalf("non-regular MIME accepted: %+v %v", cands, r.sharedMIME.err)
				}
				e.Resolver = r
				if p, _, _, err := e.FinalizeProbe("demo", ProbeResult{}, ProbeFinalize{}); err == nil || !reflect.DeepEqual(p, Profile{}) {
					t.Fatalf("invalid upper published: %+v %v", p, err)
				}
			} else {
				p := r.DeriveProfile(Profile{App: "demo"}, nil, "overlay", ReadTraceOK, cands)
				if r.sharedMIME.err != nil || len(p.Paths) != 1 || len(p.SharedWrites) != 1 {
					t.Fatalf("regular upper rejected: %+v %v", p, r.sharedMIME.err)
				}
			}
		})
	}
}

func TestSharedMIMEUpperDirectoryProbeRejected(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{".config/App/state", ".config/gnome-mimeapps.list/child"}}
	runner := &fakeRunner{}
	res, err := e.Probe(context.Background(), "testapp", ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: runner})
	if errKey(err) != "err.probe.shared_mime" || !reflect.DeepEqual(res, ProbeResult{}) {
		t.Fatalf("upper directory probe accepted: %+v %v", res, err)
	}
	if m.mounted {
		t.Fatal("mount remains")
	}
	entries, err := os.ReadDir(e.TmpRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), "appclone-probe-") {
			t.Fatalf("probe work remains: %s", en.Name())
		}
	}
}

func TestReadUpperClosedRootFails(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := readUpperRoot(root); err == nil || len(entries) != 0 {
		t.Fatalf("unknown metadata guessed: %+v %v", entries, err)
	}
}
