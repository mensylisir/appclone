package core

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"
)

type forkDecision struct {
	Path string `toml:"path"`
	Kind string `toml:"kind"`
	Data string `toml:"data,omitempty"`
}
type forkManifest struct {
	Version int            `toml:"version"`
	Paths   []forkDecision `toml:"paths"`
	Aliases []forkAlias    `toml:"aliases"`
}

var forkGenerationName = regexp.MustCompile(`^gen-[a-f0-9]{24}$`)

func forkPathHash(path string) string {
	h := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(h[:])
}

func readForkManifest(root *os.Root) (forkManifest, error) {
	m := forkManifest{Version: 1}
	f, err := root.OpenFile("current.toml", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return m, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 16<<20 {
		return m, errors.New("invalid snapshot manifest file")
	}
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil {
		return m, err
	}
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return m, err
	}
	if m.Version != 1 || len(md.Undecoded()) != 0 || !md.IsDefined("version") || !md.IsDefined("paths") {
		return m, errors.New("invalid snapshot manifest")
	}
	seen := map[string]bool{}
	for _, d := range m.Paths {
		if !filepath.IsAbs(d.Path) || filepath.Clean(d.Path) != d.Path || seen[d.Path] {
			return m, errors.New("invalid snapshot owner")
		}
		for old := range seen {
			if IsUnder(old, d.Path) || IsUnder(d.Path, old) {
				return m, errors.New("overlapping snapshot owners")
			}
		}
		seen[d.Path] = true
		switch d.Kind {
		case "snapshot":
			parts := strings.Split(d.Data, "/")
			if len(parts) != 3 || !forkGenerationName.MatchString(parts[0]) || parts[1] != forkPathHash(d.Path) || parts[2] != "data" {
				return m, errors.New("invalid snapshot reference")
			}
			if err := forkNoSymlinks(root, d.Data); err != nil {
				return m, err
			}
			fi, err := root.Lstat(d.Data)
			if err != nil {
				return m, err
			}
			if !fi.IsDir() {
				return m, errors.New("snapshot data is not a directory")
			}
		case "empty", "missing", "credential", "file":
			if d.Data != "" {
				return m, errors.New("non-snapshot decision has data")
			}
		default:
			return m, errors.New("unknown snapshot decision")
		}
	}
	aliasSeen := map[string]bool{}
	for _, a := range m.Aliases {
		if !filepath.IsAbs(a.Declared) || filepath.Clean(a.Declared) != a.Declared || aliasSeen[a.Declared] || !seen[a.Owner] {
			return m, errors.New("invalid snapshot alias")
		}
		aliasSeen[a.Declared] = true
	}
	if len(m.Paths) > 0 && len(m.Aliases) == 0 {
		return m, errors.New("snapshot identity mappings missing")
	}
	return m, nil
}

func writeForkManifest(root *os.Root, name string, m forkManifest) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

type forkSource struct {
	path   string
	root   *os.Root
	before map[string]forkNode
}

func (e *Env) forkCredentials(p Profile) ([]string, error) {
	creds, err := normalizePaths(p.CredentialPaths, e.BuildOptions())
	if err != nil {
		return nil, err
	}
	creds = append(creds, e.lexicalCredentialPaths(p.CredentialPaths)...)
	// An owner containing a shared keyring must never inherit it either.
	for _, key := range []string{"CONFIG_HOME", "DATA_HOME"} {
		keyring := filepath.Join(e.Resolver.XDG[key], "keyrings")
		creds = append(creds, keyring, Real(keyring))
	}
	for _, dir := range e.Resolver.sharedCertificateDirs() {
		creds = append(creds, dir, Real(dir))
	}
	return creds, nil
}

// prepareForkSnapshot publishes a whole generation once. Historical decisions
// remain in current.toml even after paths are removed from the current profile.
// Callers hold the instance run lock and have durably promoted the app schema.
func (e *Env) prepareForkSnapshot(p Profile, in Instance, paths, creds []string, stop func() bool) (lowersResult map[string]string, retErr error) {
	var err error
	e, in, err = e.pinForkInstance(in)
	if err != nil {
		return nil, err
	}
	aliases, err := e.forkAliases(p, paths)
	if err != nil {
		return nil, err
	}
	instPath := Real(in.Dir())
	inst, err := openForkRoot(instPath)
	if err != nil {
		return nil, err
	}
	defer inst.Close()
	if err := e.forkRun.check(); err != nil {
		return nil, err
	}
	checkSnapshotMounts := func() error {
		table, err := checkedMountTable()
		if err != nil {
			return err
		}
		return checkControlMount(table, filepath.Join(instPath, "fork-base"))
	}
	if err := checkSnapshotMounts(); err != nil {
		return nil, err
	}
	if err := inst.Mkdir("fork-base", 0o700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err := forkNoSymlinks(inst, "fork-base"); err != nil {
		return nil, err
	}
	root, err := openForkChild(inst, "fork-base")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	instInfo, err := inst.Stat(".")
	if err != nil {
		return nil, err
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	checkTarget := func() error {
		if err := checkSnapshotMounts(); err != nil {
			return err
		}
		if err := e.forkRun.check(); err != nil {
			return err
		}
		current, err := os.Lstat(instPath)
		if err != nil {
			return err
		}
		if !os.SameFile(instInfo, current) {
			return errors.New("instance directory changed")
		}
		current, err = inst.Lstat("fork-base")
		if err != nil {
			return err
		}
		if !os.SameFile(rootInfo, current) {
			return errors.New("snapshot directory changed")
		}
		return nil
	}
	if err := checkTarget(); err != nil {
		return nil, err
	}
	m, err := readForkManifest(root)
	if err != nil {
		return nil, err
	}
	aliasChanged := false
	for _, alias := range aliases {
		found := false
		for _, old := range m.Aliases {
			if old.Declared == alias.Declared {
				if old.Owner != alias.Owner {
					return nil, errInput("err.fork.scope_changed", nil)
				}
				found = true
				break
			}
		}
		if !found {
			m.Aliases = append(m.Aliases, alias)
			aliasChanged = true
		}
	}
	if err := e.cleanForkGenerations(root, instPath, m); err != nil {
		return nil, err
	}
	known := map[string]forkDecision{}
	for _, d := range m.Paths {
		known[d.Path] = d
	}
	var missing []string
	for _, path := range paths {
		if _, ok := known[path]; ok {
			continue
		}
		for old := range known {
			if IsUnder(old, path) || IsUnder(path, old) {
				return nil, errInput("err.fork.scope_changed", nil)
			}
		}
		missing = append(missing, path)
	}
	if len(missing) != 0 || aliasChanged {
		if err := forkStopped(stop); err != nil {
			return nil, err
		}
		if running, err := e.AppRunning(p.Exec); err != nil {
			return nil, err
		} else if running {
			return nil, errInput("err.fork.primary_running", nil)
		}
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		generation := "gen-" + hex.EncodeToString(nonce[:])
		stage := ".stage-" + hex.EncodeToString(nonce[:])
		if err := checkTarget(); err != nil {
			return nil, err
		}
		if err := root.Mkdir(stage, 0o700); err != nil {
			return nil, err
		}
		// Only our exclusive, unpublished staging directory can be discarded.
		defer func() {
			if err := removeForkStaging(root, filepath.Join(instPath, "fork-base"), stage, e.forkRun); err != nil {
				lowersResult, retErr = nil, forkCleanupError(err)
			}
		}()
		var sources []forkSource
		checked := map[string]*forkNode{}
		defer func() {
			for _, src := range sources {
				src.root.Close()
			}
		}()
		for _, path := range missing {
			if err := forkStopped(stop); err != nil {
				return nil, err
			}
			for _, forbidden := range []string{e.Base, e.auditRoot(), instPath, filepath.Join(instPath, "fork-base")} {
				if isUnderLoose(path, forbidden) || isUnderLoose(forbidden, path) {
					return nil, errors.New("snapshot source overlaps tool data")
				}
			}
			d := forkDecision{Path: path}
			if blocked, _ := credentialBlocksFork(path, creds); blocked {
				d.Kind = "credential"
			} else {
				fi, err := os.Lstat(path)
				if err == nil {
					n := forkIdentity(fi)
					checked[path] = &n
				} else if os.IsNotExist(err) {
					checked[path] = nil
				}
				switch {
				case os.IsNotExist(err):
					d.Kind = "missing"
				case err != nil:
					return nil, err
				case fi.Mode().IsRegular():
					d.Kind = "file"
				case !fi.IsDir():
					return nil, errors.New("snapshot source is not a directory")
				default:
					src, err := openForkRoot(path)
					if err != nil {
						return nil, err
					}
					sources = append(sources, forkSource{path: path, root: src})
					opened, err := src.Stat(".")
					if err != nil {
						return nil, err
					}
					if !os.SameFile(fi, opened) {
						return nil, errors.New("source root changed")
					}
					before, err := forkInventory(src, stop)
					if err != nil {
						return nil, err
					}
					sources[len(sources)-1].before = before
					if len(before) == 1 {
						d.Kind = "empty"
					} else {
						d.Kind = "snapshot"
						d.Data = generation + "/" + forkPathHash(path) + "/data"
					}
				}
			}
			known[path] = d
			m.Paths = append(m.Paths, d)
		}
		// All owners are inventoried before any copying begins.
		e.forkCheckpoint("before-copy")
		if err := checkTarget(); err != nil {
			return nil, err
		}
		if err := e.checkForkPaths(p, paths, aliases); err != nil {
			return nil, err
		}
		for _, src := range sources {
			d := known[src.path]
			if d.Kind != "snapshot" {
				continue
			}
			target := stage + "/" + forkPathHash(src.path) + "/data"
			if err := forkMkdirAll(root, target); err != nil {
				return nil, err
			}
			if err := forkNoSymlinks(root, target); err != nil {
				return nil, err
			}
			dst, err := openForkChild(root, target)
			if err != nil {
				return nil, err
			}
			err = copyForkTree(src.root, dst, src.before, stop, unix.IoctlFileClone)
			dst.Close()
			if err != nil {
				return nil, err
			}
			if err := forkSyncDir(root, filepath.Dir(target)); err != nil {
				return nil, err
			}
		}
		e.forkCheckpoint("before-publish")
		if err := checkTarget(); err != nil {
			return nil, err
		}
		if err := e.checkForkPaths(p, paths, aliases); err != nil {
			return nil, err
		}
		for path, before := range checked {
			fi, err := os.Lstat(path)
			if before == nil && os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if before == nil || forkIdentity(fi) != *before {
				return nil, errors.New("source decision changed before publication")
			}
		}
		for _, src := range sources {
			after, err := forkInventory(src.root, stop)
			if err != nil {
				return nil, err
			}
			fi, err := os.Lstat(src.path)
			if err != nil {
				return nil, err
			}
			if !maps.Equal(src.before, after) || forkIdentity(fi) != src.before["."] {
				return nil, errors.New("snapshot source changed before publication")
			}
		}
		if running, err := e.AppRunning(p.Exec); err != nil {
			return nil, err
		} else if running {
			return nil, errInput("err.fork.primary_running", nil)
		}
		if err := forkStopped(stop); err != nil {
			return nil, err
		}
		if err := checkTarget(); err != nil {
			return nil, err
		}
		sort.Slice(m.Paths, func(i, j int) bool { return m.Paths[i].Path < m.Paths[j].Path })
		if err := writeForkManifest(root, stage+"/manifest.toml", m); err != nil {
			return nil, err
		}
		if err := forkSyncDir(root, stage); err != nil {
			return nil, err
		}
		if err := root.Rename(stage, generation); err != nil {
			return nil, err
		}
		if err := forkSyncDir(root, "."); err != nil {
			return nil, err
		}
		tmp := ".current-" + hex.EncodeToString(nonce[:])
		defer func() {
			if err := removeForkStaging(root, filepath.Join(instPath, "fork-base"), tmp, e.forkRun); err != nil {
				lowersResult, retErr = nil, forkCleanupError(err)
			}
		}()
		if err := writeForkManifest(root, tmp, m); err != nil {
			return nil, err
		}
		if err := checkTarget(); err != nil {
			return nil, err
		}
		if err := e.checkForkPaths(p, paths, aliases); err != nil {
			return nil, err
		}
		if err := forkStopped(stop); err != nil {
			return nil, err
		}
		if err := root.Rename(tmp, "current.toml"); err != nil {
			return nil, err
		}
		if err := forkSyncDir(root, "."); err != nil {
			return nil, err
		}
	}
	if err := checkTarget(); err != nil {
		return nil, err
	}
	if err := e.checkForkPaths(p, paths, aliases); err != nil {
		return nil, err
	}
	lowers := map[string]string{}
	for _, path := range paths {
		d := known[path]
		// Current credentials override old fixed snapshots on every launch.
		if blocked, _ := credentialBlocksFork(path, creds); blocked {
			continue
		}
		if d.Kind == "snapshot" {
			lowers[path] = filepath.Join(instPath, "fork-base", d.Data)
		}
	}
	return lowers, nil
}

func forkPreparationError(err error) error {
	if errors.Is(err, ErrCanceled) {
		return err
	}
	var ce *Error
	if errors.As(err, &ce) {
		return err
	}
	return errEnv("err.fork.prepare", map[string]any{"reason": fmt.Sprint(err)}, err)
}

// A cleanup fault is actionable even if preparation was canceled. Never expose
// ErrCanceled through this error: CLI treats it as successful user cancellation.
func forkCleanupError(err error) error {
	if errors.Is(err, ErrCanceled) {
		err = errors.New(err.Error())
	}
	return errEnv("err.fork.prepare", map[string]any{"reason": fmt.Sprint(err)}, err)
}
