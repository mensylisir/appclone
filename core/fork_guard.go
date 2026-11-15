package core

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var forkTemporaryName = regexp.MustCompile(`^\.(stage|current)-[a-f0-9]{24}$`)

func (e *Env) cleanForkGenerations(root *os.Root, instPath string, m forkManifest) error {
	e.forkCheckpoint("before-generation-cleanup")
	if err := checkForkSnapshotCleanup(instPath, e.forkRun); err != nil {
		return err
	}
	if err := e.forkRun.check(); err != nil {
		return err
	}
	if err := checkedRootPath(root, filepath.Join(instPath, "fork-base")); err != nil {
		return err
	}
	used := map[string]bool{}
	for _, d := range m.Paths {
		if d.Data != "" {
			used[strings.Split(d.Data, "/")[0]] = true
		}
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		name := ent.Name()
		if used[name] || (!forkGenerationName.MatchString(name) && !forkTemporaryName.MatchString(name)) {
			continue
		}
		if err := removeForkStaging(root, filepath.Join(instPath, "fork-base"), name, e.forkRun); err != nil {
			return err
		}
	}
	return nil
}

func removeForkStaging(root *os.Root, base, name string, identity *forkRunIdentity) error {
	if err := checkForkSnapshotCleanup(filepath.Dir(base), identity); err != nil {
		return err
	}
	if err := identity.check(); err != nil {
		return err
	}
	if err := checkedRootPath(root, base); err != nil {
		return err
	}
	mounted, err := checkedMountUnder(filepath.Join(base, name))
	if err != nil {
		return err
	}
	if mounted {
		return errors.New("unpublished snapshot is mounted")
	}
	if err := checkedRootPath(root, base); err != nil {
		return err
	}
	if err := identity.check(); err != nil {
		return err
	}
	if err := checkForkSnapshotCleanup(filepath.Dir(base), identity); err != nil {
		return err
	}
	return root.RemoveAll(name)
}

func checkForkSnapshotCleanup(instPath string, identity *forkRunIdentity) error {
	table, err := checkedMountTable()
	if err != nil {
		return err
	}
	canonical, err := ordinaryPath(table, instPath, false)
	if err != nil || canonical != instPath || table[instPath] != "" {
		return errors.New("cannot inspect snapshot cleanup instance")
	}
	if err := identity.check(); err != nil {
		return err
	}
	root, err := openForkRoot(instPath)
	if err != nil {
		return err
	}
	defer root.Close()
	return checkForkStarting(root, instPath, table)
}

type forkAlias struct {
	Declared string `toml:"declared"`
	Owner    string `toml:"owner"`
}

func (e *Env) forkAliases(p Profile, owners []string) ([]forkAlias, error) {
	var out []forkAlias
	seen := map[string]bool{}
	for _, raw := range p.Paths {
		lexical, err := e.Resolver.expandLexical(raw)
		if err != nil {
			return nil, err
		}
		actual, err := e.Resolver.Expand(raw)
		if err != nil {
			return nil, err
		}
		owner := ""
		for _, path := range owners {
			if IsUnder(path, actual) {
				owner = path
				break
			}
		}
		if owner == "" {
			return nil, errInput("err.fork.scope_changed", nil)
		}
		if !seen[lexical] {
			out = append(out, forkAlias{Declared: lexical, Owner: owner})
			seen[lexical] = true
		}
	}
	return out, nil
}

func (e *Env) checkForkPaths(p Profile, owners []string, aliases []forkAlias) error {
	if err := e.forkRun.check(); err != nil {
		return err
	}
	current, err := normalizePaths(p.Paths, e.BuildOptions())
	if err != nil {
		return err
	}
	if !slices.Equal(current, owners) {
		return errInput("err.fork.scope_changed", nil)
	}
	now, err := e.forkAliases(p, owners)
	if err != nil {
		return err
	}
	if !slices.Equal(now, aliases) {
		return errInput("err.fork.scope_changed", nil)
	}
	for _, path := range owners {
		if Real(path) != path {
			return errInput("err.fork.scope_changed", nil)
		}
		for _, forbidden := range []string{e.Base, e.auditRoot()} {
			if isUnderLoose(path, forbidden) || isUnderLoose(forbidden, path) {
				return errInput("err.fork.scope_changed", nil)
			}
		}
	}
	return nil
}

func (e *Env) forkCheckpoint(stage string) {
	if e.forkTestHook != nil {
		e.forkTestHook(stage)
	}
}

// Cleanup may only rely on a complete, well-formed mount table. Unknown state
// is not evidence of no mounts. Split ASCII spaces; filenames may contain NBSP.
func checkedMountPoints() ([]string, error) {
	table, err := checkedMountTable()
	if err != nil {
		return nil, err
	}
	points := make([]string, 0, len(table))
	for point := range table {
		points = append(points, point)
	}
	return points, nil
}

func checkedMountTable() (map[string]string, error) {
	data, err := os.ReadFile(procMountInfo)
	if err != nil {
		return nil, err
	}
	points := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		f := strings.Split(line, " ")
		sep := slices.Index(f, "-")
		if len(f) < 10 || sep < 6 || len(f) != sep+4 {
			return nil, errors.New("invalid mount information")
		}
		for _, field := range f {
			if field == "" || strings.ContainsAny(field, "\t\r\x00") {
				return nil, errors.New("invalid mount field")
			}
		}
		for _, i := range []int{0, 1} {
			if !mountDecimal(f[i]) {
				return nil, errors.New("invalid mount ID")
			}
		}
		device := strings.Split(f[2], ":")
		if len(device) != 2 {
			return nil, errors.New("invalid mount device")
		}
		for _, part := range device {
			if !mountDecimal(part) {
				return nil, errors.New("invalid mount device")
			}
		}
		// nsfs roots may be net:[inode], rather than filesystem paths.
		if f[3] == "" || !strings.HasPrefix(f[4], "/") {
			return nil, errors.New("invalid mount path")
		}
		for _, index := range []int{3, 4, sep + 2} {
			if !validMountEscapes(f[index]) {
				return nil, errors.New("invalid mount path escape")
			}
		}
		point := unescapeMountPath(f[4])
		if !filepath.IsAbs(point) || filepath.Clean(point) != point {
			return nil, errors.New("noncanonical mount path")
		}
		// Stacked mounts are legal. Keep every record so replacements or an
		// extra layer also change the identity checked by the runtime guard.
		points[point] += line + "\n"
	}
	return points, nil
}

func mountDecimal(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

func validMountEscapes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		if i+4 > len(s) {
			return false
		}
		switch s[i : i+4] {
		case `\040`, `\011`, `\012`, `\134`:
			i += 3
		default:
			return false
		}
	}
	return true
}

func checkedMountUnder(base string) (bool, error) {
	table, err := checkedMountTable()
	if err != nil {
		return false, err
	}
	canonical, err := ordinaryPath(table, base, false)
	if err != nil {
		return false, err
	}
	for point := range table {
		for _, form := range []string{base, canonical} {
			if IsUnder(form, point) {
				return true, nil
			}
		}
	}
	return false, nil
}

// Create one component at a time, opening each with a checked confined handle.
// No mkdir follows an existing symlink, including a link to elsewhere in root.
func forkMkdirAll(root *os.Root, path string) error {
	if !filepath.IsLocal(path) {
		return errors.New("invalid snapshot subdirectory")
	}
	parent := root
	var opened []*os.Root
	defer func() {
		for _, r := range opened {
			r.Close()
		}
	}()
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		if err := parent.Mkdir(part, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		fi, err := parent.Lstat(part)
		if err != nil {
			return err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("mount directory is not a plain directory")
		}
		next, err := parent.OpenRoot(part)
		if err != nil {
			return err
		}
		opened = append(opened, next)
		got, err := next.Stat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(fi, got) {
			return errors.New("mount directory changed")
		}
		parent = next
	}
	return nil
}
