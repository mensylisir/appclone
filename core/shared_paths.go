package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sharedMIMEPaths belongs to one synchronous classification operation. Errors
// are sticky: a later successful stat cannot repair already incomplete evidence.
type sharedMIMEPaths struct {
	files []string
	err   error
}

func sharedMIMEName(name string) bool {
	return name == "mimeapps.list" || (strings.HasSuffix(name, "-mimeapps.list") && name != "-mimeapps.list")
}

func (r *PathResolver) withSharedMIME() *PathResolver {
	if r.sharedMIME != nil {
		return r
	}
	copy := *r
	copy.sharedMIME = &sharedMIMEPaths{}
	config := r.XDG["CONFIG_HOME"]
	entries, err := os.ReadDir(config)
	if err != nil {
		if os.IsNotExist(err) {
			if _, lerr := os.Lstat(config); os.IsNotExist(lerr) {
				return &copy
			}
		}
		copy.sharedMIME.err = errEnv("err.env.read_dir", map[string]any{"path": config}, err)
		return &copy
	}
	for _, en := range entries {
		if sharedMIMEName(en.Name()) {
			path := filepath.Join(config, en.Name())
			copy.sharedMIME.files = append(copy.sharedMIME.files, path)
			copy.checkSharedMIMEFile(path, false)
		}
	}
	return &copy
}

func (r *PathResolver) checkSharedMIMEFile(path string, allowMissing bool) bool {
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
		return true
	}
	if err != nil && allowMissing && os.IsNotExist(err) {
		if _, lerr := os.Lstat(path); os.IsNotExist(lerr) {
			return true
		}
	}
	if err == nil {
		err = fmt.Errorf("MIME association target is not a regular file")
	}
	if r.sharedMIME.err == nil {
		r.sharedMIME.err = errEnv("err.probe.shared_mime", map[string]any{"path": path}, err)
	}
	return false
}

// sharedCertificateDirs are common NSS stores, separate from Secret Service.
func (r *PathResolver) sharedCertificateDirs() []string {
	return []string{filepath.Join(r.Home, ".pki"), filepath.Join(r.XDG["DATA_HOME"], "pki")}
}

// preciseSharedBoundary identifies desktop-wide state without promoting an
// application owner to shared when a common directory points inside that app.
func (r *PathResolver) preciseSharedBoundary(abs string) (string, bool) {
	r = r.withSharedMIME()
	dirs := append(r.sharedCertificateDirs(), filepath.Join(r.XDG["DATA_HOME"], "applications"))
	for _, dir := range dirs {
		if isUnderLoose(dir, abs) {
			return Real(dir), true
		}
	}
	return r.preciseSharedMIMEBoundary(abs)
}

func (r *PathResolver) preciseSharedMIMEBoundary(abs string) (string, bool) {
	r = r.withSharedMIME()
	config := r.XDG["CONFIG_HOME"]
	mime := filepath.Join(config, "mimeapps.list")
	if samePathLoose(mime, abs) && r.checkSharedMIMEFile(mime, true) {
		return Real(mime), true
	}
	if base, rel, ok := looseRel(config, abs); ok && rel != "" && !strings.ContainsRune(rel, filepath.Separator) &&
		sharedMIMEName(rel) {
		file := filepath.Join(base, rel)
		if r.checkSharedMIMEFile(file, true) {
			return Real(file), true
		}
	}
	for _, file := range r.sharedMIME.files {
		if samePathLoose(file, abs) && r.checkSharedMIMEFile(file, false) {
			return Real(file), true
		}
	}
	return "", false
}

// HasSharedCertificates reports the adopted profile's shared certificate scope.
// It does not infer sharing from a suggestion or the independent keyring toggle.
func (r *PathResolver) HasSharedCertificates(p Profile) bool {
	for _, raw := range p.SharedWrites {
		abs, err := r.Expand(raw)
		if err != nil {
			continue
		}
		for _, dir := range r.sharedCertificateDirs() {
			if isUnderLoose(dir, abs) || isUnderLoose(abs, dir) {
				return true
			}
		}
	}
	return false
}
