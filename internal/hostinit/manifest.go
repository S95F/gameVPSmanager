// Package hostinit installs gvm onto a host: the binary, the systemd units,
// the tmpfiles and sysusers fragments, and a default host config. It is the
// code behind `sudo gvm host init`, and it is the one place that knows where
// each packaged file belongs on disk.
//
// That "one place" is the point. `make install` and the Phase 52 .deb both
// build their file list from Manifest() here, so the day a unit is added there
// is exactly one table to add it to and no second list to drift. The
// installer is deliberately not in package packaging: packaging holds the
// files and the assertions about their contents; where they land on a host is
// an install-time decision that depends on this binary, not on them.
package hostinit

import (
	"io/fs"
	"path"
	"sort"

	"github.com/S95F/gameVPSmanager/packaging"
)

// Kind groups an installed file by the post-write action it needs.
type Kind int

const (
	// KindUnit is a systemd unit, timer or slice. Writing one dirties the
	// manager, so the install reloads systemd once after the whole set.
	KindUnit Kind = iota
	// KindTmpfiles is a tmpfiles.d fragment, applied with `systemd-tmpfiles
	// --create <path>` so the directories it declares exist before anything
	// runs.
	KindTmpfiles
	// KindSysusers is a sysusers.d fragment, applied with `systemd-sysusers` so
	// the accounts the units run as exist before they start.
	KindSysusers
)

// File is one packaged file and where it is installed.
//
// Src is a path inside packaging.FS; Dest is absolute on the host. Mode is the
// installed permission. Nothing here is owner/group: every file this installer
// writes is root-owned (the units, tmpfiles and sysusers are all read by root
// systemd), and the one group-readable file on the box — an instance spec — is
// written by `gvm apply`, not by the installer.
type File struct {
	Src  string
	Dest string
	Mode fs.FileMode
	Kind Kind
}

// Install directories. Package files go under /usr/lib, never /usr/local:
// Debian policy forbids a package shipping /usr/local/bin, and /usr/lib is
// where a distro expects a package's own units and fragments to live (D-41 is
// the path decision; the operator never sees it).
const (
	unitDir     = "/usr/lib/systemd/system"
	tmpfilesDir = "/usr/lib/tmpfiles.d"
	sysusersDir = "/usr/lib/sysusers.d"

	// BinaryPath is the one path the units hard-code and the one place the
	// binary is installed. Phase 23 item 1 moved every unit here.
	BinaryPath = "/usr/bin/gvm"

	// ConfigPath is the host config `gvm host init` writes once, if absent.
	ConfigPath = "/etc/gvm/gvm.yaml"

	// MarkerPath records that the first init happened, so the timers it enables
	// are enabled once and an operator who later disables them is respected.
	MarkerPath = "/var/lib/gvm/host-init.json"
)

// dashboardSrcs are the packaged files that belong to the dashboard alone,
// installed only by `host init --dashboard` (Phase 48 item 3): the two
// daemons, the explicit migration oneshot, and the caddy-group tmpfiles lines
// whose group would otherwise fail `systemd-tmpfiles --create` on a
// games-only host (onboarding-1).
var dashboardSrcs = map[string]bool{
	"systemd/gvmd.service":           true,
	"systemd/gvm-web.service":        true,
	"systemd/gvm-db-migrate.service": true,
	"tmpfiles/gvm-web.conf":          true,
}

// Manifest is every packaged file a games-only install writes, with where it
// goes. DashboardManifest returns the rest.
//
// It is derived from packaging.FS by walking it, so a unit added to
// packaging/systemd appears here the moment it is embedded — there is no
// second list of unit names to keep in step (a DASHBOARD unit is the one
// deliberate exception: it must also be named in dashboardSrcs, and
// TestManifestCoversEveryPackagedFile fails when the two sets do not
// partition the embed). The order is stable (sorted by destination) so
// `--diff` and the install log read the same every time.
func Manifest() []File {
	return walkManifest(func(src string) bool { return !dashboardSrcs[src] })
}

// DashboardManifest is the dashboard-only remainder, installed with
// `host init --dashboard`.
func DashboardManifest() []File {
	return walkManifest(func(src string) bool { return dashboardSrcs[src] })
}

func walkManifest(keep func(src string) bool) []File {
	var out []File
	add := func(dir string, k Kind) {
		entries, err := packaging.FS.ReadDir(dir)
		if err != nil {
			// packaging.FS is embedded at build time; a missing directory is a
			// build-the-binary-wrong bug, not a host condition, and the test
			// TestManifestCoversEveryPackagedFile turns it red on the dev
			// machine. Returning nothing here keeps Manifest total.
			return
		}
		var destDir string
		switch k {
		case KindUnit:
			destDir = unitDir
		case KindTmpfiles:
			destDir = tmpfilesDir
		case KindSysusers:
			destDir = sysusersDir
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			src := path.Join(dir, e.Name())
			if !keep(src) {
				continue
			}
			out = append(out, File{
				Src:  src,
				Dest: path.Join(destDir, e.Name()),
				Mode: 0o644,
				Kind: k,
			})
		}
	}
	add("systemd", KindUnit)
	add("tmpfiles", KindTmpfiles)
	add("sysusers.d", KindSysusers)

	sort.Slice(out, func(i, j int) bool { return out[i].Dest < out[j].Dest })
	return out
}

// firstTimers are the timers `gvm host init` enables on the first init only.
//
// They have no control-plane dependency — the guard reconciles units and the
// verify timer runs a read-only check — so enabling them on a games-only box
// is safe and is what makes the box self-healing without an operator wiring a
// cron. On a re-init they are left exactly as the operator last set them.
var firstTimers = []string{"gvm-guard.timer", "gvm-job@_verify.timer"}

// FirstTimers returns the timers enabled on the first init, in a copy the
// caller may not mutate the backing of.
func FirstTimers() []string { return append([]string(nil), firstTimers...) }
