// `gvm install` — the preflight for the provider seam, and an honest refusal.
//
// The provider (SteamCMD and friends) is phase 8. What CAN be answered today is
// everything that must be true before a provider runs, and answering it here is
// worth doing rather than deferring: an operator who has just run `gvm create`
// and `gvm apply` wants to know whether the next step will work, and every
// condition below is one that would otherwise be discovered by a half-finished
// 65 GB download.
//
// The refusal at the end names the phase rather than pretending. A command that
// exited 0 having installed nothing would be the worst possible answer.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// ErrProviderUnbuilt is `gvm install` reaching the seam that is not here yet.
//
// It is its own sentinel, and deliberately NOT ErrNotImplemented: the command
// is built, its preflight ran and reported, and what is missing is one
// downstream package. A script distinguishing the two is distinguishing
// "gvm cannot do this at all" from "gvm cannot do this yet".
var ErrProviderUnbuilt = errors.New("gvm: the provider seam is not built in this release")

type installOptions struct {
	Name   string
	Repair bool
	Remote string
}

func parseInstallArgs(env *Env, args []string) (installOptions, error) {
	var opt installOptions
	fs := flag.NewFlagSet("gvm install", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm install <instance>\n\n"+
			"Installs the game files an instance's manifest declares.\n\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&opt.Repair, "repair", false, "re-validate an existing install rather than assuming it is intact")
	fs.StringVar(&opt.Remote, "remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return installOptions{}, flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return installOptions{}, err
	}
	opt.Name = name
	return opt, nil
}

func runInstall(ctx context.Context, env *Env, args []string) error {
	opt, err := parseInstallArgs(env, args)
	if err != nil {
		return err
	}
	t, err := Resolve(env, "install", opt.Remote)
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}
	h, err := env.host(ctx)
	if err != nil {
		return err
	}

	in, _, err := loadInstance(h, opt.Name)
	if err != nil {
		return err
	}
	m, err := pinnedManifest(h, in)
	if err != nil {
		return err
	}

	// managed_install:false is the ADOPTION flag and it is a refusal, not a
	// warning. The files under /opt/pzserver predate gvm and belong to a
	// running world; a provider that ran app_update against them would
	// overwrite an install nobody asked it to touch.
	if !in.Spec.ManagedInstall {
		return fmt.Errorf("%s is adopted (managed_install: false): gvm never installs, updates "+
			"or validates its files. That is what adoption means", opt.Name)
	}

	present, why := sentinelState(h, in, m)
	printInstallPreflight(env.Stdout, in, m, present, why)

	if present && !opt.Repair {
		fmt.Fprintf(env.Stdout, "\n  %s is already installed. `gvm install %s --repair` "+
			"re-validates it.\n", opt.Name, opt.Name)
		return nil
	}
	return fmt.Errorf("%w: %s declares provider %q, whose driver lands with the blob store. "+
		"Everything above this line was checked and holds",
		ErrProviderUnbuilt, m.Metadata.ID, m.Spec.Provider.Kind)
}

// sentinelState answers "is this installed" the way the manifest defines it.
//
// Not "is the install directory non-empty": a directory holding 40 GB of a
// 65 GB download is neither empty nor installed, and the difference is a
// server that starts and immediately dies. A manifest that declares no
// sentinel gets "cannot tell", which is reported as such.
func sentinelState(h *HostEnv, in *spec.Instance, m *spec.Manifest) (bool, string) {
	s, declared := sentinelOf(m)
	if !declared || s.Path == "" {
		return false, "this manifest declares no install sentinel, so gvm cannot tell whether " +
			"the files are there"
	}
	p := path.Join(in.Spec.Paths.Install, s.Path)
	fi, err := h.files().Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, p + " is absent"
	case err != nil:
		return false, fmt.Sprintf("%s could not be checked: %v", p, err)
	case fi.IsDir():
		return false, p + " is a directory, and the sentinel names a file"
	case s.MinBytes > 0 && fi.Size() < s.MinBytes:
		return false, fmt.Sprintf("%s is %s, under the manifest's %s floor: a partial download",
			p, spec.FormatBytes(fi.Size()), spec.FormatBytes(s.MinBytes))
	}
	return true, p + " is present"
}

func printInstallPreflight(w io.Writer, in *spec.Instance, m *spec.Manifest, present bool, why string) {
	fmt.Fprintf(w, "\ninstall %s\n", in.Metadata.Name)
	fmt.Fprintf(w, "  game      %s rev %d (%s), pinned\n",
		m.Metadata.ID, m.Metadata.Revision, m.Origin)
	fmt.Fprintf(w, "  provider  %s\n", m.Spec.Provider.Kind)
	fmt.Fprintf(w, "  install   %s\n", in.Spec.Paths.Install)
	fmt.Fprintf(w, "  as        %s\n", in.Spec.User)
	fmt.Fprintf(w, "  steam HOME %s  (per instance, so two steamcmd cannot race on ~/.steam)\n",
		in.Spec.Paths.Work)
	state := "not installed"
	if present {
		state = "installed"
	}
	fmt.Fprintf(w, "  sentinel  %s — %s\n", state, why)
}

// sentinelOf reads the sentinel out of whichever provider block is populated.
//
// It returns a second value rather than a zero Sentinel so that "this manifest
// declares nothing to look for" (adopted, none) cannot be mistaken for "the
// file it names is missing". The first is unknowable and the second is a broken
// install, and printing them the same way is how an adopted instance gets
// reported as never installed.
func sentinelOf(m *spec.Manifest) (spec.Sentinel, bool) {
	switch p := m.Spec.Provider; {
	case p.SteamCMD != nil:
		return p.SteamCMD.Sentinel, true
	case p.Minecraft != nil:
		return p.Minecraft.Sentinel, true
	}
	return spec.Sentinel{}, false
}
