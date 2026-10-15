package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// A preserve sidecar is how gvm pins an archive it must never prune, without
// touching the archive's own bytes.
//
// `gvm destroy` authorises itself with an archive, and that archive must still
// exist long after — long enough to actually restore from if the destroy was a
// mistake. Retention protects class-preserve archives, but the authorising
// archive is usually a routine nightly, and the three obvious ways to protect
// it all fail: rewriting the class inside the tarball changes its sha256;
// flipping the index entry does not survive Reindex, which re-reads Class from
// the manifest; and a per-instance rule cannot cover a NEW instance created
// with the same name into the same backup directory. A root-owned 0600 sidecar
// beside the archive survives all three: Reindex reads it on every rescan and
// forces Class=preserve, and the file is tied to the archive's basename, not to
// any instance. Phase 36's typed `gvm backup delete` is the only thing that
// removes a pin.
const PreserveSidecarExt = ".preserve"

// PreservePin is the sidecar's contents: why, when, and for which instance.
type PreservePin struct {
	Reason   string    `json:"reason"`
	PinnedAt time.Time `json:"pinned_at"`
	Instance string    `json:"instance"`
}

// PreserveSidecarPath is the sidecar beside an archive. It ends in a suffix the
// index scanner skips (it is not Ext), so a sidecar is never mistaken for an
// archive.
func PreserveSidecarPath(archivePath string) string {
	return archivePath + PreserveSidecarExt
}

// WritePreservePin writes the sidecar atomically at 0600. It never overwrites
// an existing pin with a weaker one: a second destroy against the same archive
// leaves the first reason in place, and re-pinning is idempotent.
func WritePreservePin(archivePath string, pin PreservePin) error {
	if _, ok, err := ReadPreservePin(archivePath); err == nil && ok {
		return nil
	}
	b, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// 0600 and root-owned: a pin an unprivileged process could remove would not
	// be the guarantee it is meant to be.
	return spec.WriteFileAtomicNoBackup(PreserveSidecarPath(archivePath), b, 0o600)
}

// ReadPreservePin reads the sidecar. ok is false when there is none, which is
// the ordinary case for an archive nothing has pinned.
func ReadPreservePin(archivePath string) (PreservePin, bool, error) {
	b, err := os.ReadFile(PreserveSidecarPath(archivePath))
	if errors.Is(err, fs.ErrNotExist) {
		return PreservePin{}, false, nil
	}
	if err != nil {
		return PreservePin{}, false, err
	}
	var pin PreservePin
	if err := json.Unmarshal(b, &pin); err != nil {
		return PreservePin{}, false, fmt.Errorf("archive: parse %s: %w",
			PreserveSidecarPath(archivePath), err)
	}
	return pin, true, nil
}
