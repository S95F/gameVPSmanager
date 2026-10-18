package curseforge

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DropBoxSuffix is the file extension gvm looks for when scanning a drop-box.
// The operator places JAR files here when a mod author has opted out of API
// distribution and the CDN URL is therefore absent from the API response.
const DropBoxSuffix = ".jar"

// DropBoxDir is the single, host-wide directory an operator drops manually
// obtained jars into for a blocked mod. It is one shared location, not a
// per-instance one: an operator who downloads a jar by hand should not have to
// know which instance's private directory to put it in, and the fingerprint
// match (never the path) is what ties a dropped jar to the instance that needs
// it.
//
// It is exported and referenced by name everywhere the path appears — the
// resolver that scans it, the `gvm content blocked` report that names it, and
// the docs — so the three cannot drift into telling an operator to use one
// directory while gvm reads another. TestDropBoxDirIsTheDocumentedPath pins the
// value, and the CLI uses this symbol rather than a literal of its own.
const DropBoxDir = "/var/lib/gvm/downloads/mods"

// blockedEntry records the state of a blocked mod.
type blockedEntry struct {
	ProjectID int
	FileID    int
	FileName  string // best-effort from the API, may be empty
}

// isBlocked reports whether a cfFile's download URL is absent, which means the
// mod author opted out of API distribution. A nil file is treated as blocked.
func isBlocked(f *cfFile) bool {
	return f == nil || strings.TrimSpace(f.DownloadURL) == ""
}

// ScanDropBox scans dir for files matching the given fingerprint (murmur2) or
// base filename. It returns the absolute path of the first match, or "" if no
// match is found.
//
// The primary match key is the murmur2 fingerprint. The fallback is the bare
// filename (case-insensitive, without extension comparison on the stem). This
// lets an operator rename "SomeLib-1.20.1.jar" to "some-lib.jar" and still
// have gvm pick it up.
func ScanDropBox(dir string, fingerprint uint32, filename string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("curseforge: scan drop-box %s: %w", dir, err)
	}

	stemWant := fileStem(filename)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), DropBoxSuffix) {
			continue
		}
		full := filepath.Join(dir, e.Name())

		// Primary: match by murmur2 fingerprint.
		if fingerprint != 0 {
			data, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			if murmur2(data) == fingerprint {
				return full, nil
			}
		}

		// Fallback: match by filename stem.
		if stemWant != "" && strings.EqualFold(fileStem(e.Name()), stemWant) {
			return full, nil
		}
	}
	return "", nil
}

// WalkDropBox enumerates all JAR files under dir (non-recursive).
// It returns their absolute paths and murmur2 fingerprints.
func WalkDropBox(dir string) ([]DropBoxFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("curseforge: walk drop-box %s: %w", dir, err)
	}
	var out []DropBoxFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), DropBoxSuffix) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		out = append(out, DropBoxFile{
			Path:        full,
			Name:        e.Name(),
			Fingerprint: murmur2(data),
			Size:        int64(len(data)),
		})
	}
	return out, nil
}

// DropBoxFile is a JAR found in the drop-box with its fingerprint.
type DropBoxFile struct {
	Path        string
	Name        string
	Fingerprint uint32
	Size        int64
}

// fileStem returns the filename without extension.
func fileStem(name string) string {
	name = filepath.Base(name)
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

// ErrNoDropBox is returned when the drop-box directory does not exist.
var ErrNoDropBox = fmt.Errorf("curseforge: drop-box directory not found; create it or run gvm content blocked")

// _ is a compile-time assertion that fs.DirEntry is used.
var _ fs.DirEntry
