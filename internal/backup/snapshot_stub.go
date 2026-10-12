//go:build !linux

package backup

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// The non-Linux half: a refusal, not a pretence.
//
// It would be easy to have Probe return an empty Capability and no error here,
// because Choose already answers `direct` for an empty one and the development
// machine would then look like a host with an unremarkable filesystem. That is
// exactly the wrong shape. An engine that cannot tell "this box has ext4" from
// "nobody asked" would hide a real probe failure on a real VPS behind the same
// silence, and the cost of being wrong in that direction is a seventy-second
// freeze nobody was warned about.
//
// So the stub refuses, and the engine's handling of the refusal -- record the
// reason, choose `direct`, carry on -- is the behaviour that gets tested on
// Windows. That is the harder path and the one that runs on most VPS images,
// so having it be the one the development machine exercises is a feature.

// Probe is Linux-only.
func Probe(worldPaths []string, snapDir string) (Capability, error) {
	return Capability{
			Note: "the filesystem could not be probed on this operating system, so the archive " +
				"is taken inside the freeze",
		},
		platform.Unsupported("statfs and FICLONE")
}

// Clone is Linux-only. Nothing here falls back to a byte copy: a "clone" that
// copied would be a snapshot whose window is the whole copy while the result
// claimed to be instantaneous, and the stall figure in the job record would be
// a lie.
func Clone(ctx context.Context, m Method, src, dst string) ([]string, error) {
	return nil, platform.Unsupported("FICLONE and btrfs subvolume snapshots")
}
