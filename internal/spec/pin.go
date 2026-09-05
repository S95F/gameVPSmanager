package spec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The pinned manifest lives at /etc/gvm/instances/<name>.d/manifest.lock.yaml
// (D-04). Not under /var/lib: the revision the operator accepted is INTENT, and
// intent is git-able, diffable and not something a "safe to lose" directory may
// own.
const (
	PinDirSuffix = ".d"
	PinFileName  = "manifest.lock.yaml"
)

var (
	// ErrNoPin means the instance has never been applied — there is no pinned
	// manifest yet.
	ErrNoPin = errors.New("no pinned manifest")

	// ErrPinMismatch means the pinned bytes are not the ones the instance says
	// it runs. The runner exits 64 on this rather than starting the server
	// (D-17); anything else silently runs argv nobody approved.
	ErrPinMismatch = errors.New("pinned manifest does not match the instance pin")

	// ErrNoRawBytes means a manifest was asked to be pinned without the bytes it
	// was loaded from.
	ErrNoRawBytes = errors.New("manifest has no source bytes to pin")
)

// InstanceDropInDir is <etcInstances>/<name>.d, the per-instance directory that
// holds the pin.
func InstanceDropInDir(etcInstancesDir, name string) string {
	return filepath.Join(etcInstancesDir, name+PinDirSuffix)
}

// PinPath is the pinned manifest inside an instance's drop-in directory.
func PinPath(instanceDir string) string { return filepath.Join(instanceDir, PinFileName) }

// WritePin copies a manifest's VERBATIM bytes into the instance's drop-in
// directory (D-17).
//
// The bytes are copied, never re-marshalled. A re-marshalled manifest is a
// different file: it has a different sha256, so it can no longer be compared
// against the approval that authorised it, and yaml.v3 would rewrite comments,
// key order and every duration into its own spelling — turning `gvm manifest
// diff` into pages of churn that hides the one line that matters.
func WritePin(instanceDir string, m *Manifest) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrNoRawBytes)
	}
	if len(m.Raw) == 0 {
		return fmt.Errorf("%w: %s", ErrNoRawBytes, m.Metadata.ID)
	}
	// A caller that edited Raw without re-checksumming would pin bytes whose
	// recorded sha describes something else, and the mismatch would not surface
	// until the runner refused to start.
	if m.SHA256 != "" && !strings.EqualFold(m.SHA256, sha256Hex(m.Raw)) {
		return fmt.Errorf("%w: manifest sha256 %s does not describe its own bytes",
			ErrPinMismatch, shortSHA(m.SHA256))
	}
	if err := os.MkdirAll(instanceDir, ModeSpecDir); err != nil {
		return fmt.Errorf("create %s: %w", instanceDir, err)
	}
	return WriteFileAtomic(PinPath(instanceDir), m.Raw, ModeSpec)
}

// LoadPinned reads the pinned manifest. The runner, the job runner and the
// backup engine use ONLY this, never the catalog: upgrading the package on disk
// must not change a live instance's argv, environment, shutdown ladder or
// destroy globs (D-17).
//
// The returned Manifest has an EMPTY Origin. The pinned copy is not one of the
// three origins — it is a copy of whichever one won at apply time, and the
// instance's own ManifestPin.Origin records which. Leaving it empty means a
// pinned file can never be mistaken for an approved override.
func LoadPinned(instanceDir string) (*Manifest, error) {
	path := PinPath(instanceDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoPin, path)
		}
		return nil, err
	}
	m, _, err := parseManifest(path, raw)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// PinOf is the pin an instance YAML should record for a loaded manifest.
func PinOf(m *Manifest) ManifestPin {
	return ManifestPin{
		Revision: m.Metadata.Revision,
		SHA256:   m.SHA256,
		Origin:   m.Origin,
	}
}

// Verify compares a loaded manifest against what the instance says it runs.
//
// The sha256 is the real check; the revision is compared too because a revision
// that matches with a different sha is a manifest someone edited in place
// without bumping it, and that is worth a different sentence in the failure than
// an honest upgrade.
func (p ManifestPin) Verify(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("%w: no manifest", ErrPinMismatch)
	}
	if p.SHA256 == "" {
		return fmt.Errorf("%w: the instance records no sha256", ErrPinMismatch)
	}
	got := m.SHA256
	if got == "" {
		got = sha256Hex(m.Raw)
	}
	if !strings.EqualFold(p.SHA256, got) {
		if p.Revision == m.Metadata.Revision {
			return fmt.Errorf("%w: revision %d, but sha256 %s != pinned %s: "+
				"the manifest was edited in place without bumping metadata.revision",
				ErrPinMismatch, p.Revision, shortSHA(got), shortSHA(p.SHA256))
		}
		return fmt.Errorf("%w: pinned revision %d sha256 %s, got revision %d sha256 %s",
			ErrPinMismatch, p.Revision, shortSHA(p.SHA256), m.Metadata.Revision, shortSHA(got))
	}
	if p.Revision != m.Metadata.Revision {
		return fmt.Errorf("%w: identical bytes carry revision %d but the instance pinned %d",
			ErrPinMismatch, m.Metadata.Revision, p.Revision)
	}
	return nil
}

// LoadPinnedFor reads the pin and checks it against the instance's recorded
// ManifestPin in one call, which is what every runtime caller actually wants:
// reading the pinned file and forgetting to verify it is the same bug as not
// pinning at all.
func LoadPinnedFor(instanceDir string, pin ManifestPin) (*Manifest, error) {
	m, err := LoadPinned(instanceDir)
	if err != nil {
		return nil, err
	}
	if err := pin.Verify(m); err != nil {
		return nil, fmt.Errorf("%s: %w", PinPath(instanceDir), err)
	}
	return m, nil
}
